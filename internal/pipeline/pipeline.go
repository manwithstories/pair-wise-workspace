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
		perStream, expanders, wrapEvents, clockErrs := expandClocks(m, rec)
		all = append(all, clockErrs...)
		var composeErrs []model.Error
		tl, composeErrs = timeline.Compose(m, perStream, expanders, wrapEvents)
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
		wrapCount := countWraps(m, rec)
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
// propagate into verification evidence.
func expandClocks(m *manifest.Manifest, rec *transport.Result) (
	map[int][]manifest.Packet, map[int]*clock.WrapExpander,
	map[int][]clock.WrapEvent, []model.Error) {

	perStream := make(map[int][]manifest.Packet)
	expanders := make(map[int]*clock.WrapExpander)
	wrapEvents := make(map[int][]clock.WrapEvent)
	var errs []model.Error

	for i := range m.Streams {
		s := &m.Streams[i]
		var exp *clock.WrapExpander
		if s.WrapPeriod > 0 {
			exp, _ = clock.NewWrapExpander(s.ID, s.WrapPeriod)
		}
		expanders[s.ID] = exp
		var streamPkts []manifest.Packet
		for _, p := range rec.Packets {
			if p.StreamID != s.ID {
				continue
			}
			streamPkts = append(streamPkts, p)
			if exp != nil && p.Kind == manifest.KindMedia {
				_, ev, err := exp.Expand(p.RawPTS, p.SourceOffset)
				if err != nil {
					errs = append(errs, model.NewError(model.StageClock, s.ID,
						model.ErrIntegerOverflow, model.SeverityFatal,
						err.Error(), p.SourceOffset, p.RawPTS, true))
					continue
				}
				if ev != nil {
					wrapEvents[s.ID] = append(wrapEvents[s.ID], *ev)
					errs = append(errs, model.NewError(model.StageClock, s.ID,
						model.ErrPTSWrap, model.SeverityRecovered,
						fmt.Sprintf("wrap #%d raw_pts=%d", ev.WrapCount, ev.RawPTS),
						p.SourceOffset, p.RawPTS, true))
				}
			}
		}
		perStream[s.ID] = streamPkts
	}
	return perStream, expanders, wrapEvents, errs
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

func countWraps(m *manifest.Manifest, rec *transport.Result) int {
	count := 0
	for i := range m.Streams {
		s := &m.Streams[i]
		if s.WrapPeriod <= 0 {
			continue
		}
		exp, _ := clock.NewWrapExpander(s.ID, s.WrapPeriod)
		for _, p := range rec.Packets {
			if p.StreamID != s.ID || p.Kind != manifest.KindMedia {
				continue
			}
			if _, ev, err := exp.Expand(p.RawPTS, p.SourceOffset); err == nil && ev != nil {
				count++
			}
		}
	}
	return count
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
