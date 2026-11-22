// Package app wires ingest, timeline, inspection, metadata, RTP
// windowing, repair, the bounded pipeline and remux into one run.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"mediacheck/internal/clock"
	"mediacheck/internal/fixture"
	"mediacheck/internal/inspect"
	"mediacheck/internal/metadata"
	"mediacheck/internal/model"
	rtpwin "mediacheck/internal/rtp"
)

// Run executes one end-to-end check against a generated package.
func Run(ctx context.Context, opts Options) (Result, error) {
	if err := validate(opts); err != nil {
		return Result{Exit: model.ExitInvalidArgs}, err
	}
	start := time.Now()
	memBytes := opts.MemoryMiB << 20

	// 1. Generate the synthetic delivery package (bounded read stand-in).
	cfg := fixture.Config{
		Profile: opts.Profile, DurationMs: opts.DurationMs, LossBurst: opts.LossBurst,
		VideoTS: 90000, AudioTS: 48000, DriftMs: opts.DriftMs,
	}
	pkg, err := fixture.Generate(cfg)
	if err != nil {
		return Result{Exit: model.ExitInvalidArgs}, err
	}
	if int64(len(pkg.MP4)) > memBytes {
		return Result{Exit: model.ExitInvalidArgs},
			fmt.Errorf("input %d bytes exceeds memory limit %d", len(pkg.MP4), memBytes)
	}

	// 2. Bounded ingest of the MP4 bytes.
	r, err := newBoundedReader(pkg.MP4, memBytes)
	if err != nil {
		return Result{Exit: model.ExitInternal}, err
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		if errors.Is(err, ErrLimitSentinel) {
			return Result{Exit: model.ExitInvalidArgs}, err
		}
		return Result{Exit: model.ExitInternal}, err
	}

	// 3. Box structure inspection on the unified reader.
	boxRep, err := inspect.WalkBoxes(bytes.NewReader(raw), int64(len(raw)), memBytes)
	if err != nil {
		return Result{Exit: model.ExitInternal}, err
	}
	tl := clock.New()
	tl.AddTrack("video", cfg.VideoTS)
	tl.AddTrack("audio", cfg.AudioTS)

	allFaults := append([]model.Fault{}, boxRep.Faults...)
	allFaults = append(allFaults, tl.Faults()...)

	// 4. Per-track sample and keyframe analysis.
	policy := inspect.KeyframePolicy{MaxIntervalMs: opts.MaxKfGapMs}
	trackEnds := map[string]int64{}
	var items []Item
	for _, tr := range []string{"video", "audio"} {
		samples := pkg.Samples[tr]
		rep := inspect.AnalyzeTrack(samples, policy)
		nextID := faultIDGen(&allFaults)
		allFaults = append(allFaults, inspect.TrackFaults(rep, nextID)...)
		if len(samples) > 0 {
			trackEnds[tr] = samples[len(samples)-1].StartMs + samples[len(samples)-1].DurMs
		}
		for _, s := range samples {
			items = append(items, Item{
				Track: tr, StartMs: s.StartMs, EndMs: s.StartMs + s.DurMs,
				Bytes: s.Size, Sample: s, EnqueuedAt: start,
			})
		}
	}

	// 5. RTP window: dedup, reorder, gap accounting.
	win, err := rtpwin.NewWindow(opts.WindowCap)
	if err != nil {
		return Result{Exit: model.ExitInternal}, err
	}
	feedRTP(win, pkg.RTPPackets, &allFaults, pkg.RTPGap)
	rtpMetrics := win.Metrics()

	// 6. Timed metadata alignment.
	alignRep := metadata.Align(pkg.Events, pkg.Windows, metadata.DefaultOptions())
	allFaults = append(allFaults, alignRep.Faults...)

	// 7. Cross-track clock drift.
	if drift := maxDrift(pkg); drift > 80 {
		allFaults = append(allFaults, model.Fault{
			ID: len(allFaults) + 1, Kind: model.KindClockDrift,
			Severity: model.SeverityError, Track: "audio",
			StartMs: 0, EndMs: trackEnds["audio"],
			Detail:   fmt.Sprintf("cross-track drift %dms exceeds 80ms", drift),
			Recovery: model.RecPending,
		})
	}

	// 8. Repair / adjudication.
	repairRes := repairAll(allFaults, trackEnds)

	// 9. Bounded pipeline with backpressure to the slow remux sink.
	out, peak, p95, err := runPipeline(ctx, items, repairRes, opts, memBytes, start)
	if err != nil {
		return Result{Exit: model.ExitInternal, Committed: repairRes.Boundaries}, err
	}

	// 10. Summary metrics.
	sum := buildSummary(opts, pkg, rtpMetrics, alignRep, repairRes, peak, p95, len(raw), len(out))
	res := Result{
		Summary: sum, Faults: repairRes.Faults, Segments: repairRes.Segments,
		Output: out, Committed: repairRes.Boundaries,
	}
	res.Exit = decideExit(repairBridge{inner: repairRes})
	return res, nil
}

func decideExit(r repairResultLike) model.ExitCode {
	if r.HasUnrecovered() {
		return model.ExitDamaged
	}
	for _, s := range r.Segments() {
		if s.Recovery == model.RecQuarantined {
			return model.ExitDamaged
		}
	}
	return model.ExitOK
}

// feedRTP pushes every packet, injects a duplicate to prove the window
// counts it once, and emits one aggregated RTP fault per gap span. The
// fault's recovery state reflects whether the window filled the hole.
func feedRTP(win *rtpwin.Window, packets []*rtpPacket, faults *[]model.Fault, gap fixture.Span) {
	for _, p := range packets {
		if _, err := win.Push(p); err != nil && errors.Is(err, rtpwin.ErrWindowOverflow) {
			*faults = append(*faults, model.Fault{
				ID: len(*faults) + 1, Kind: model.KindBufferOverflow,
				Severity: model.SeverityError, Track: "video",
				StartMs: gap.StartMs, EndMs: gap.EndMs,
				Detail: err.Error(), Recovery: model.RecPending,
			})
		}
	}
	if len(packets) > 0 {
		// Re-push the first packet as a duplicate (counted once).
		_, _ = win.Push(packets[0])
	}
	win.Flush()
	m := win.Metrics()
	if m.GapPackets == 0 {
		return
	}
	rec := model.RecPending
	detail := fmt.Sprintf("RTP gap %d sequences, %d restored, %d lost",
		m.GapPackets, m.Recovered, m.GapPackets-m.Recovered)
	if m.Recovered >= m.GapPackets {
		rec = model.RecRecovered
	}
	*faults = append(*faults, model.Fault{
		ID: len(*faults) + 1, Kind: model.KindRTPGap,
		Severity: model.SeverityError, Track: "video",
		StartMs: gap.StartMs, EndMs: gap.EndMs,
		Detail: detail, Recovery: rec,
	})
}
