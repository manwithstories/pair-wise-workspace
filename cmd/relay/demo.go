package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"relay/internal/fixture"
	"relay/internal/index"
	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/output"
	"relay/internal/stream"
	"relay/internal/timeline"
	"relay/internal/verify"
)

// execute runs the full deterministic demo and prints the acceptance line.
func execute(opts options) error {
	ctx := context.Background()
	sc := fixture.BuildAllScenario()
	if sc.Generated != 25 {
		return &codedError{exitWrite, fmt.Sprintf("fixture generated %d, want 25", sc.Generated)}
	}

	// ---- Stage 1: ingest every delivered file, isolating parse faults. ----
	parsed, isolated, err := ingestAll(sc)
	if err != nil {
		return &codedError{exitWrite, err.Error()}
	}
	graph := timeline.NewGraph(specsFrom(sc))
	cycleRejected := graph.CycleRejected()

	// ---- Stage 2: repair tracks in topological order, honoring failures. ----
	reports, err := repairTracks(graph, parsed)
	if err != nil {
		return &codedError{exitWrite, err.Error()}
	}
	deduped := 0
	accepted := 0
	for _, r := range reports {
		deduped += r.Deduped
		accepted += len(r.Accepted)
	}
	graph.PropagateFailures()
	if err := publishGraph(graph, reports); err != nil {
		return &codedError{exitVerify, err.Error()}
	}

	// ---- Stage 3: full reference run (clean, no interruption). ----
	for _, d := range []string{opts.refDir, opts.outDir} {
		if err := os.RemoveAll(d); err != nil {
			return &codedError{exitWrite, err.Error()}
		}
	}
	refPipe, err := stream.New(
		stream.Config{OutDir: opts.refDir, QueueBytes: opts.queueBytes},
		output.NewWriter())
	if err != nil {
		return &codedError{exitWrite, err.Error()}
	}
	if _, err := refPipe.Run(ctx, reports); err != nil {
		return &codedError{exitWrite, "reference run: " + err.Error()}
	}

	// ---- Stage 4: interrupted run followed by resume. ----
	peak, err := interruptedResume(ctx, opts, reports)
	if err != nil {
		return &codedError{exitWrite, err.Error()}
	}

	// ---- Stage 5: independent verification re-reading disk outputs. ----
	resumePipe, err := openResume(opts)
	if err != nil {
		return &codedError{exitWrite, err.Error()}
	}
	result, err := verifyAll(opts, reports, refPipe, resumePipe)
	if err != nil {
		return &codedError{exitVerify, err.Error()}
	}

	printAcceptance(sc, accepted, deduped, isolated, cycleRejected,
		opts.queueBytes, peak, result)
	return nil
}

// ingestAll streams each fixture file through the ingest reader and splits
// results into healthy fragments versus isolated parse faults.
func ingestAll(sc *fixture.Scenario) (map[uint32][]*ingest.Fragment, int, error) {
	parsed := map[uint32][]*ingest.Fragment{}
	isolated := 0
	for _, in := range sc.Files {
		rd := ingest.NewReader(bytes.NewReader(in.Bytes), in.Name, int64(len(in.Bytes)))
		ev, err := rd.Next()
		if err != nil {
			return nil, 0, err
		}
		if ev.Fault != nil {
			if !ev.Fault.Isolation() {
				return nil, 0, fmt.Errorf("unexpected non-isolating fault: %v", ev.Fault)
			}
			isolated++
			continue
		}
		parsed[ev.Fragment.TrackID] = append(parsed[ev.Fragment.TrackID], ev.Fragment)
	}
	return parsed, isolated, nil
}

func specsFrom(sc *fixture.Scenario) []timeline.TrackSpec {
	out := make([]timeline.TrackSpec, 0, len(sc.Tracks))
	for _, t := range sc.Tracks {
		out = append(out, timeline.TrackSpec{
			ID: t.ID, Timescale: t.Timescale, DependsOn: t.DependsOn,
		})
	}
	return out
}

func repairTracks(graph *timeline.Graph, parsed map[uint32][]*ingest.Fragment) ([]*timeline.TrackReport, error) {
	reports := make([]*timeline.TrackReport, 0)
	for _, id := range graph.Order() {
		node, ok := graph.Node(id)
		if !ok || node.Reason != nil {
			continue // cyclic/missing-dep tracks never enter repair
		}
		clock := media.Clock{TrackID: id, Timescale: node.Spec.Timescale}
		rep := timeline.RepairTrack(id, clock, parsed[id])
		reports = append(reports, rep)
	}
	return reports, nil
}

// publishGraph marks each repaired track published or failed. A failed track
// blocks its dependents via PropagateFailures but never stops independent ones.
func publishGraph(graph *timeline.Graph, reports []*timeline.TrackReport) error {
	byID := map[uint32]*timeline.TrackReport{}
	for _, r := range reports {
		byID[r.TrackID] = r
	}
	for _, id := range graph.Order() {
		node, _ := graph.Node(id)
		if node.Reason != nil {
			continue
		}
		r := byID[id]
		if r == nil || r.Fault != nil {
			var f *media.Fault
			if r != nil {
				f = r.Fault
			}
			graph.MarkPublished(id, f)
			continue
		}
		graph.MarkPublished(id, nil)
	}
	return nil
}

func openResume(opts options) (*stream.Pipeline, error) {
	return stream.New(
		stream.Config{OutDir: opts.outDir, QueueBytes: opts.queueBytes},
		output.NewWriter())
}

// interruptedResume performs the interrupted first run and the resuming
// second run, returning the highest queue peak observed across both.
func interruptedResume(ctx context.Context, opts options,
	reports []*timeline.TrackReport) (int64, error) {
	if !opts.resume || opts.interruptAfter <= 0 {
		p, err := stream.New(
			stream.Config{OutDir: opts.outDir, QueueBytes: opts.queueBytes,
				ConsumerDelay: opts.consumerDelay}, output.NewWriter())
		if err != nil {
			return 0, err
		}
		if _, err := p.Run(ctx, reports); err != nil {
			return 0, err
		}
		return p.PeakQueueBytes(), p.Close()
	}
	first, err := stream.New(
		stream.Config{OutDir: opts.outDir, QueueBytes: opts.queueBytes,
			ConsumerDelay: opts.consumerDelay,
			InterruptAfter: opts.interruptAfter}, output.NewWriter())
	if err != nil {
		return 0, err
	}
	if _, err := first.Run(ctx, reports); err != stream.ErrInterrupted {
		return 0, fmt.Errorf("expected interruption, got %v", err)
	}
	peak := first.PeakQueueBytes()
	if err := first.Close(); err != nil {
		return 0, err
	}
	second, err := stream.New(
		stream.Config{OutDir: opts.outDir, QueueBytes: opts.queueBytes,
			ConsumerDelay: opts.consumerDelay}, output.NewWriter())
	if err != nil {
		return 0, err
	}
	if _, err := second.Run(ctx, reports); err != nil {
		return 0, err
	}
	if v := second.PeakQueueBytes(); v > peak {
		peak = v
	}
	return peak, second.Close()
}

// verifyAll runs every independent check and aggregates the printed metrics.
func verifyAll(opts options, reports []*timeline.TrackReport,
	refPipe, resumePipe *stream.Pipeline) (verification, error) {
	var res verification

	rt, err := verify.RoundTrip(opts.outDir, resumePipe.Checkpoint())
	if err != nil {
		return res, err
	}
	res.roundtrip = rt.Rate
	if rt.Failed != 0 {
		return res, fmt.Errorf("roundtrip failures: %v", rt.Faults)
	}

	matched, total, fault := verify.Hashes(opts.outDir, resumePipe.Checkpoint())
	if fault != nil {
		return res, fault
	}
	res.hashMatched, res.hashTotal = matched, total

	resMatch, resTotal, fault := verify.Resume(refPipe.Checkpoint(), resumePipe.Checkpoint())
	if fault != nil {
		return res, fault
	}
	res.resumeMatched, res.resumeTotal = resMatch, resTotal

	b := &index.Builder{}
	for _, r := range reports {
		for _, rf := range r.Accepted {
			if err := b.AddFragment(rf, opts.outDir); err != nil {
				return res, err
			}
		}
	}
	idx := b.Build()
	if err := index.Write(idx, filepath.Join(opts.outDir, "index.json")); err != nil {
		return res, err
	}
	hits, expected, fault := verify.Keyframes(idx, videoReports(reports))
	if fault != nil {
		return res, fault
	}
	res.keyHits, res.keyExpected = hits, expected

	if err := refPipe.Close(); err != nil {
		return res, err
	}
	if err := resumePipe.Close(); err != nil {
		return res, err
	}
	return res, nil
}

func videoReports(reports []*timeline.TrackReport) []*timeline.TrackReport {
	out := make([]*timeline.TrackReport, 0)
	for _, r := range reports {
		if r.TrackID == 101 {
			out = append(out, r)
		}
	}
	return out
}
