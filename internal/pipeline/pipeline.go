// Package pipeline wires the three subsystem groups into one deterministic
// run: clock/timeline consume manifest packets with source offsets, the
// recovered packets flow into transform and frame export, and verify reads
// the unified timeline, outputs and error records.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"bq/internal/clock"
	"bq/internal/fixture"
	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/render"
	"bq/internal/timeline"
	"bq/internal/transform"
	"bq/internal/transport"
	"bq/internal/verify"
)

// RunResult is everything the CLI needs to print or persist.
type RunResult struct {
	Manifest   *manifest.Manifest
	Timeline   *timeline.Timeline
	Drift      []timeline.DriftReport
	Transport  *transport.Result
	Frames     []render.FrameResult
	Metrics    verify.Metrics
	Report     verify.Report
	AllErrors  []model.Error
	Latencies  []verify.StageLatency
	FrameHash  string
}

// Runner executes the stages while recording per-stage latency.
type Runner struct {
	outDir string
	th     verify.Thresholds
}

// NewRunner builds a pipeline writing beneath outDir.
func NewRunner(outDir string) *Runner {
	return &Runner{outDir: outDir, th: verify.DefaultThresholds()}
}

// Run executes the full pipeline against an already-generated manifest.
func (r *Runner) Run(ctx context.Context, m *manifest.Manifest) (*RunResult, []model.Error) {
	var all []model.Error
	var latencies []verify.StageLatency

	// Re-validate the in-memory manifest so a structurally invalid document
	// (unsupported codec, bad timebase) aborts with a fatal record.
	if fatalErrs := manifest.Validate(m); len(fatalErrs) > 0 {
		model.SortErrors(fatalErrs)
		report := verify.BuildReport(verify.Metrics{
			ErrorCount: len(fatalErrs), FirstSourceOffset: -1,
		}, fatalErrs, r.th, 0, 0)
		return &RunResult{Manifest: m, Report: report, AllErrors: fatalErrs}, fatalErrs
	}

	timed := func(stage string, fn func() []model.Error) {
		start := time.Now()
		errs := fn()
		latencies = append(latencies, verify.StageLatency{
			Stage: stage, Ms: time.Since(start).Milliseconds(),
		})
		all = append(all, errs...)
	}

	// Stage 1: delivery simulation + XOR recovery.
	var delivered []manifest.Packet
	var rec *transport.Result
	timed("transport", func() []model.Error {
		delivered = transport.SimulateDelivery(m)
		rec = transport.Recover(m, delivered)
		return rec.Errors
	})

	// Stage 2: clock expansion (wrap) then timeline composition.
	var tl *timeline.Timeline
	var drift []timeline.DriftReport
	timed("clock", func() []model.Error {
		perStream, expanders, wrapEvents, preExpanded, clockErrs := expandClocks(m, rec)
		all = append(all, clockErrs...)
		var composeErrs []model.Error
		tl, composeErrs = timeline.Compose(m, perStream, expanders, wrapEvents, preExpanded)
		return composeErrs
	})
	timed("timeline", func() []model.Error {
		var driftErrs []model.Error
		drift, driftErrs = timeline.EstimateDrift(m, tl)
		return driftErrs
	})

	// Stage 3: per-track remux (independent tracks, failures isolated).
	timed("transform", func() []model.Error {
		remuxer := transform.New(m, tl, r.outDir)
		_, remuxErrs := remuxer.Run(ctx)
		return remuxErrs
	})

	// Stage 4: deterministic off-screen frame export.
	var frames []render.FrameResult
	frameHash := ""
	timed("render", func() []model.Error {
		var renderErrs []model.Error
		frames, frameHash, renderErrs = r.exportFrames(m, tl, drift)
		return renderErrs
	})

	// Stage 5: verification over the unified evidence.
	var report verify.Report
	timed("verify", func() []model.Error {
		// Wrap count comes straight from the clock-stage evidence, so the
		// verifier never re-derives (and potentially disagrees with) it.
		wrapCount := countCodes(all, model.ErrPTSWrap)
		unrec := countUnrecoverable(drift)
		in := verify.Input{
			Manifest: m, Timeline: tl, Drift: drift, Transport: rec,
			Latencies: latencies, FrameHash: frameHash, FrameCount: len(frames),
		}
		metrics, _ := verify.Compute(in)
		report = verify.BuildReport(metrics, all, r.th, wrapCount, unrec)
		return nil
	})

	model.SortErrors(all)
	return &RunResult{
		Manifest: m, Timeline: tl, Drift: drift, Transport: rec,
		Frames: frames, Metrics: report.Metrics, Report: report,
		AllErrors: all, Latencies: latencies, FrameHash: frameHash,
	}, all
}

// expandClocks runs the per-stream wrap expander over recovered media and
// emits a PTS_WRAP record for every detected wraparound so wrap events
// propagate into verification evidence. It also returns each media packet's
// base-corrected, unwrapped source ticks (keyed by source offset) so the
// timeline composer never has to re-expand.
func expandClocks(m *manifest.Manifest, rec *transport.Result) (
	map[int][]manifest.Packet, map[int]*clock.WrapExpander,
	map[int][]clock.WrapEvent, map[int]map[int64]int64, []model.Error) {

	perStream := make(map[int][]manifest.Packet)
	expanders := make(map[int]*clock.WrapExpander)
	wrapEvents := make(map[int][]clock.WrapEvent)
	preExpanded := make(map[int]map[int64]int64)
	var errs []model.Error

	for i := range m.Streams {
		s := &m.Streams[i]
		var exp *clock.WrapExpander
		if s.WrapPeriod > 0 {
			// Base is the zero-phase origin expressed in SOURCE ticks (the
			// stream start PTS); expansion stays on the source axis and is only
			// converted to master ticks later by timeline.Compose.
			exp, _ = clock.NewWrapExpanderWithBase(s.ID, s.WrapPeriod, s.StartPTS)
		}
		expanders[s.ID] = exp
		// Recovery appends reconstructed packets after the delivered block,
		// so re-establish per-stream sequence order before the wrap expander
		// sees the stream: a monotonic expander must never read out-of-order
		// PTS values. Parity packets follow the media they belong to.
		streamPkts := sortedStreamPackets(rec.Packets, s.ID)
		expandedMap := make(map[int64]int64)
		for _, p := range streamPkts {
			if p.Kind != manifest.KindMedia {
				continue
			}
			if exp == nil {
				// No wrap: still remove the fixed start-PTS phase so every
				// stream shares the zero-phase source origin.
				expandedMap[p.SourceOffset] = p.RawPTS - s.StartPTS
				continue
			}
			full, ev, err := exp.Expand(p.RawPTS, p.SourceOffset)
			if err != nil {
				errs = append(errs, model.NewError(model.StageClock, s.ID,
					model.ErrIntegerOverflow, model.SeverityFatal,
					err.Error(), p.SourceOffset, p.RawPTS, true))
				continue
			}
			expandedMap[p.SourceOffset] = full
			if ev != nil {
				wrapEvents[s.ID] = append(wrapEvents[s.ID], *ev)
				errs = append(errs, model.NewError(model.StageClock, s.ID,
					model.ErrPTSWrap, model.SeverityRecovered,
					fmt.Sprintf("wrap #%d raw_pts=%d", ev.WrapCount, ev.RawPTS),
					p.SourceOffset, p.RawPTS, true))
			}
		}
		preExpanded[s.ID] = expandedMap
		perStream[s.ID] = streamPkts
	}
	return perStream, expanders, wrapEvents, preExpanded, errs
}

// sortedStreamPackets returns one stream's packets in deterministic order:
// media packets first by ascending sequence, then parity packets. This is the
// monotonic view the clock and timeline subsystems require.
func sortedStreamPackets(all []manifest.Packet, streamID int) []manifest.Packet {
	var media, parity []manifest.Packet
	for _, p := range all {
		if p.StreamID != streamID {
			continue
		}
		if p.Kind == manifest.KindParity {
			parity = append(parity, p)
			continue
		}
		media = append(media, p)
	}
	sort.SliceStable(media, func(i, j int) bool { return media[i].Seq < media[j].Seq })
	sort.SliceStable(parity, func(i, j int) bool { return parity[i].GroupID < parity[j].GroupID })
	return append(media, parity...)
}

func (r *Runner) exportFrames(m *manifest.Manifest, tl *timeline.Timeline,
	drift []timeline.DriftReport) ([]render.FrameResult, string, []model.Error) {

	reqs := render.DefaultRequests(m.DurationS)
	var all []render.FrameResult
	var renderErrs []model.Error
	hasher := sha256.New()
	for i := range m.Streams {
		s := &m.Streams[i]
		if s.Kind != manifest.KindVideo {
			continue
		}
		exp := render.NewExporter(*s, tl.Streams[s.ID], drift, r.outDir, m.Seed)
		results, errs := exp.Export(reqs)
		renderErrs = append(renderErrs, errs...)
		all = append(all, results...)
		for _, fr := range results {
			hasher.Write([]byte(fr.SHA256))
		}
	}
	combined := hex.EncodeToString(hasher.Sum(nil))
	return all, combined, renderErrs
}

// countCodes counts collected error records carrying a given code.
func countCodes(errs []model.Error, code string) int {
	n := 0
	for _, e := range errs {
		if e.Code == code {
			n++
		}
	}
	return n
}

func countUnrecoverable(reports []timeline.DriftReport) int {
	n := 0
	for _, rep := range reports {
		n += len(rep.Unrecoverable)
	}
	return n
}

// Generate is a convenience wrapper used by the demo command.
func Generate(ctx context.Context, opts fixture.Options, outDir string) (
	*manifest.Manifest, error) {
	m := fixture.Build(opts)
	if err := fixture.Generate(ctx, m, outDir); err != nil {
		return nil, err
	}
	return m, nil
}
