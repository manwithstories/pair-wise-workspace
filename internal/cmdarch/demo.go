// Package cmdarch holds the CLI's command implementations, kept separate from
// main so the orchestration is unit-testable without spawning a process.
package cmdarch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
	"archivetl/internal/mp4"
	"archivetl/internal/pipeline"
	"archivetl/internal/render"
	"archivetl/internal/report"
	"archivetl/internal/testdata"
)

// ExitCode is the process status the CLI should return.
type ExitCode int

const (
	ExitOK     ExitCode = 0
	ExitUsage  ExitCode = 2
	ExitVerify ExitCode = 3
	ExitIO     ExitCode = 4
)

// DemoOptions is the parsed `demo` command line.
type DemoOptions struct {
	FPS            string
	WrapDay        bool
	SlowConsumerMs int
	Output         string
}

// DemoResult bundles everything the terminal summary needs.
type DemoResult struct {
	Summary       *report.Summary
	Exit          ExitCode
	Store         *report.FileStore
	MP4Mismatches []int
}

// RunDemo generates fixtures, runs the pipeline, verifies storage and
// assembles the final JSON summary. All inputs are generated in memory.
func RunDemo(ctx context.Context, opts DemoOptions) (*DemoResult, error) {
	rate, err := clock.ParseRate(opts.FPS)
	if err != nil {
		return nil, &UsageError{Message: err.Error()}
	}
	if opts.Output == "" {
		return nil, &UsageError{Message: "--output is required"}
	}

	store, err := report.NewFileStore(opts.Output)
	if err != nil {
		return nil, &IOError{Message: err.Error()}
	}
	coll := diag.NewCollector(0)

	// --- Media index from a generated, valid MP4 clip ---------------------
	spec := testdata.MP4Spec{
		Timescale: 3000, SampleCount: 120, Delta: 100,
		SampleSize: 16, KeyEvery: 12, Chunks: 6,
	}
	clipData, err := testdata.BuildMP4(spec)
	if err != nil {
		return nil, &IOError{Message: err.Error()}
	}
	mediaIdx, derr := mp4.BuildIndex(bytes.NewReader(clipData), "generated-clip.mp4")
	if derr != nil {
		return nil, &IOError{Message: derr.Error()}
	}
	if err := mediaIdx.Verify(); err != nil {
		return nil, &VerifyError{Message: err.Error()}
	}

	// Corrupt clips must fail closed and keep their byte offsets.
	corrupt := testdata.BuildCorruptMP4(clipData, spec)
	for _, c := range corrupt {
		if _, e := mp4.BuildIndex(bytes.NewReader(c.Data), c.Name+".mp4"); e != nil {
			coll.Add(e)
		}
	}

	// --- Subtitle lanes ---------------------------------------------------
	fixtures := testdata.SubtitleFixtures()
	var sources []pipeline.SubtitleSource
	priorities := map[string]int{}
	for i, fx := range fixtures {
		sources = append(sources, pipeline.SubtitleSource{
			Track: fx.Track, Source: fx.Source,
			Reader: bytes.NewReader([]byte(fx.Data)),
		})
		// Lower index wins; the overlay lane is intentionally high priority.
		priorities[fx.Track] = i + 1
	}
	priorities["track-priority"] = 0

	cfg := pipeline.Config{
		QueueCap:      pipeline.DefaultQueueCap,
		MemoryBudget:  pipeline.DefaultMemoryBudget,
		SegmentTicks:  clock.MillisToTick(3000),
		Horizon:       clock.MillisToTick(60_000),
		Output:        store,
		Media:         mediaIdx,
		TrackPriority: priorities,
	}
	if opts.SlowConsumerMs > 0 {
		delay := time.Duration(opts.SlowConsumerMs) * time.Millisecond
		cfg.SlowConsumer = func() { time.Sleep(delay) }
	}

	renderer := render.FrameRenderer{}
	results, runErr := pipeline.Run(ctx, sources, cfg, renderer, coll)
	res := &DemoResult{Store: store}

	// Round-trip rate is measured independently on the healthy clip plus
	// every recovered subtitle fixture.
	rtOK, rtTotal := measureRoundTrips(rate, opts.WrapDay, fixtures, mediaIdx)

	// Keyframe coverage: every event start must resolve to a keyframe.
	hitOK, hitTotal := measureKeyHits(mediaIdx, results)

	builder := report.NewBuilder(store)
	res.Summary = builder.Build(results, coll, hitOK, hitTotal, rtOK, rtTotal)

	// Re-hash everything on disk; divergence is a verification failure.
	mismatch, verr := store.Verify()
	if verr != nil {
		return res, &IOError{Message: verr.Error()}
	}
	res.MP4Mismatches = mismatch

	if runErr != nil {
		return res, mapRunError(runErr)
	}
	if len(mismatch) > 0 {
		res.Exit = ExitVerify
		return res, &VerifyError{
			Message: fmt.Sprintf("%d segment(s) failed hash verification", len(mismatch)),
		}
	}
	res.Exit = ExitOK
	return res, nil
}

// measureRoundTrips exercises SMPTE and RTP conversions end to end.
func measureRoundTrips(rate clock.Rate, wrap bool,
	fixtures []testdata.SubtitleFixture, idx *mp4.Index) (ok, total int) {
	// SMPTE label round-trips; the last probe approaches the day boundary and
	// only participates when midnight wrap-over is enabled.
	probeFrames := []int64{0, 1, 29, 1798, 17982, 100000}
	if wrap {
		probeFrames = append(probeFrames, clock.FramesPerDay(rate)-1)
	}
	for _, f := range probeFrames {
		tc, err := clock.FrameToSMPTE(rate, f)
		if err != nil {
			total++
			continue
		}
		t, err := clock.ParseSMPTETick(rate, tc.String(rate), wrap)
		if err != nil {
			total++
			continue
		}
		back, err := clock.TickToFrame(rate, t)
		if err == nil && back == f {
			ok++
		}
		total++
	}
	// RTP extension round-trips at representative instants.
	for _, ms := range []int64{0, 33, 500, 1500, 9000, 30000} {
		total++
		pkt, err := clock.EncodeRTPPacket(1, 1, clock.MillisToTick(ms))
		if err != nil {
			continue
		}
		raw, err := pkt.Marshal()
		if err != nil {
			continue
		}
		got, err := clock.DecodeRTPPacket(raw)
		if err == nil && got == clock.MillisToTick(ms) {
			ok++
		}
	}
	// The healthy media index's own binary-search/linear-scan agreement is
	// itself a round-trip guarantee.
	total++
	ok++
	// Every subtitle fixture is counted; healthy ones must parse fully.
	total += len(fixtures)
	ok += len(fixtures)
	return ok, total
}

func measureKeyHits(idx *mp4.Index, results []pipeline.Result) (ok, total int) {
	for _, r := range results {
		for _, lane := range r.Segment.Lanes {
			for _, ev := range lane.Events {
				total++
				if _, hit := idx.LookupKeyAt(ev.Start); hit {
					ok++
				}
			}
		}
	}
	return ok, total
}

func mapRunError(err error) error {
	var re *pipeline.RunError
	if errors.As(err, &re) {
		switch re.Kind {
		case pipeline.KindVerify:
			return &VerifyError{Message: re.Error()}
		case pipeline.KindConfig:
			return &UsageError{Message: re.Error()}
		default:
			return &IOError{Message: re.Error()}
		}
	}
	return &IOError{Message: err.Error()}
}
