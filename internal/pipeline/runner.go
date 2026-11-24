package pipeline

import (
	"os"
	"path/filepath"

	"mediasim/internal/clock"
	"mediasim/internal/fixture"
	"mediasim/internal/graph"
	"mediasim/internal/mp4"
	"mediasim/internal/render"
)

// Result is everything the CLI needs to print and exit with.
type Result struct {
	Report   *Report
	Manifest *fixture.Manifest
	Hashes   []render.FrameHash
	Errors   *clock.List
	Plan     *graph.Plan
}

// Run executes the full local pipeline. Recoverable damage never aborts the
// run: the returned report carries a non-zero exit code instead.
func Run(cfg Config) (*Result, error) {
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, clock.NewError(clock.CatArgument, clock.Span{}, clock.RecoveredNone, "create out dir", err)
	}
	path, man, err := fixture.Ensure(filepath.Join(cfg.OutDir, "fixtures"), cfg.Fixture)
	if err != nil {
		return nil, clock.NewError(clock.CatInternal, clock.Span{}, clock.RecoveredNone, "build fixture", err)
	}
	timeline, plan, err := buildGraphPlan()
	if err != nil {
		return nil, err
	}
	// The plan drives parse/recovery order: segments are processed in their
	// dependency order, and the report records that order.
	segs, err := planSegments(plan)
	if err != nil {
		return nil, err
	}

	info, err := mp4.Parse(path)
	if err != nil {
		return nil, err
	}
	rec, err := mp4.Recover(path, info)
	if err != nil {
		return nil, err
	}
	// Only segments the fixture actually generated participate in recovery;
	// a plan that references a source the container lacks is a graph error.
	for _, seg := range segs {
		if !sourcePresent(info, seg) {
			return nil, clock.Errorf(clock.CatGraph,
				clock.NewSpan(seg.Track, seg.Segment, clock.Interval{Start: seg.Start, End: seg.Start + seg.Duration}),
				clock.RecoveredNone, "planned segment has no track in the container")
		}
	}

	transport := simulateTransport(cfg, info, rec)
	rep := &Report{Fixture: cfg.Fixture, Seed: cfg.Seed}
	rep.RTP = transport.RTP
	rep.Recovery, _ = recoverySummary(info, rec)
	rep.CorruptRanges = len(info.Gaps)
	// Indexed frames come from an intact sample table; a scan recovers the
	// rest when the index is gone. The two are disjoint.
	if info.Indexed {
		rep.RecoveredIndexed = recoveredPrefix(info, nil)
		rep.RecoveredPrefix = rep.RecoveredIndexed
	} else {
		rep.RecoveredPrefix = recoveredPrefix(info, rec)
	}
	rep.PacketRecovery = packetRecoveryRate(transport.RTP)

	// Render only frames the receiver actually delivered, in output order.
	errs := &clock.List{}
	out, err := renderFrames(cfg.OutDir, cfg.Seed, cfg, info, rec, transport, errs)
	if err != nil {
		return nil, err
	}
	rep.Frames = out.frames
	rep.FrameHashes = len(out.hashes)
	rep.AVSyncP95Ms = Percentile(out.sync, 0.95)
	rep.AVSyncMaxMs = Percentile(out.sync, 1)
	rep.LatencyP95Ms = Percentile(out.latencies, 0.95)
	// Cut audio at every planned edit point using the fixed silence policy,
	// and publish each cut with its residual synchronisation error.
	_, audioEdits, editErr := applyEditPoints(timeline, plan)
	if editErr != nil {
		return nil, editErr
	}
	rep.AudioEdits = audioEdits
	_, gapEntries := recoverySummary(info, rec)
	rep.Errors = append(rep.Errors, gapEntries...)
	rep.Errors = append(rep.Errors, errorEntries(errs)...)

	// The honest expectation is the number of frames whose origin bytes are
	// intact; anything else missing is a defect, not a recovery.
	expected := expectedRenderable(info, rec)
	rep.Finish(cfg.Gates)
	rep.Gates.FramesOK = rep.Frames == expected
	rep.Gates.HashesOK = rep.FrameHashes == rep.Frames
	rep.ExitCode = decideExit(rep, expected, info)
	_ = man

	return &Result{Report: rep, Manifest: man, Hashes: out.hashes, Errors: errs, Plan: plan}, nil
}
