package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"fieldcut/internal/clock"
	"fieldcut/internal/cut"
	"fieldcut/internal/events"
	"fieldcut/internal/fingerprint"
	"fieldcut/internal/graph"
	"fieldcut/internal/ingest"
	"fieldcut/internal/manifest"
	"fieldcut/internal/recover"
)

const ExitInterrupt = 75

// Result is the terminal-facing pipeline result.
type Result struct {
	Code         int
	ReusedNodes  []string
	OutputSHA256 string
	Manifest     *manifest.Manifest
	FirstError   *clock.Error
}

// Run executes the deterministic pipeline. Damaged inputs produce a Result
// with Code=2 after preserving committable work; unexpected errors return err.
func Run(req Request) (*Result, error) {
	if req.OutDir == "" {
		return nil, clock.Errorf(clock.CodeConfig, -1, "output directory is required")
	}
	if req.Case == "" {
		req.Case = "clean"
	}
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return nil, err
	}
	reqBytes, _ := json.MarshalIndent(req, "", "  ")
	if err := recover.AtomicWriteFile(filepath.Join(req.OutDir, "request.json"), append(reqBytes, '\n')); err != nil {
		return nil, err
	}

	j, err := recover.Open(req.OutDir)
	if err != nil {
		return nil, err
	}
	bundle, err := Generate(req)
	if err != nil {
		return nil, err
	}
	stageHashes := stageInputHashes(bundle)
	reusedAtStart := j.ReusedNodes(stageHashes)

	if !j.IsCommitted("generate", stageHashes["generate"]) {
		if err := writeInputs(req.OutDir, bundle); err != nil {
			return nil, err
		}
		if err := j.Commit("generate", stageHashes["generate"], nil); err != nil {
			return nil, err
		}
	}

	director, ingestErr := ingest.ReadFragmentedMP4(bundle.DirectorBytes, 1)
	if ingestErr != nil {
		_ = j.Fail("ingest", stageHashes["ingest"], ingestErr)
		if ce, ok := clock.AsError(ingestErr); ok && ce.Code == clock.CodeTruncatedMP4 {
			return runDamaged(req, j, bundle, director, ce, stageHashes, reusedAtStart)
		}
		return nil, ingestErr
	}
	stageTrack, err := ingest.ReadFragmentedMP4(bundle.StageBytes, 2)
	if err != nil {
		return nil, err
	}
	if err := j.Commit("ingest", stageHashes["ingest"], nil); err != nil {
		return nil, err
	}

	aligned, alignErrs := events.Align(bundle.RawEvents)
	crossErrs := events.CrossTrackSkew(aligned, 5*clock.NsPerMs)
	allErrs := append(append([]*clock.Error{}, alignErrs...), crossErrs...)
	if err := j.Commit("align", stageHashes["align"], nil); err != nil {
		return nil, err
	}

	colorDecision, err := graph.ResolveColor([]graph.ColorDescription{
		colorToGraph(director.Color, 1), colorToGraph(stageTrack.Color, 2),
	}, graph.ColorPolicy{Mode: "prefer_track", PreferredTrackID: 1})
	if err != nil {
		return nil, err
	}
	gp, order, err := buildGraph()
	if err != nil {
		return nil, err
	}
	_ = gp
	if err := j.Commit("graph", stageHashes["graph"], map[string]string{"topo_order": HashBytes([]byte(fmtJoin(order)))}); err != nil {
		return nil, err
	}

	primaryPCM, primaryMetrics, silence, audioReports, err := processAudio(bundle)
	if err != nil {
		return nil, err
	}
	if err := j.Commit("audio", stageHashes["audio"], audioReports); err != nil {
		return nil, err
	}

	candidates := cutCandidates(aligned, silence, director)
	cutPlan, err := cut.PlanCuts(cut.Request{
		TrackID: 1, Duration: Duration, Candidates: candidates,
		AudioSampleRate: OutputRate, VideoFPS: VideoFPS, Errors: allErrs,
	})
	if err != nil {
		return nil, err
	}
	if err := cut.Coverage(cutPlan.Segments, Duration); err != nil {
		return nil, err
	}
	assignments, err := events.Assign(aligned, cutPlan.Segments)
	if err != nil {
		return nil, err
	}
	if err := j.Commit("cut", stageHashes["cut"], nil); err != nil {
		return nil, err
	}

	windows := fingerprintWindows(director, stageTrack, cutPlan.Segments)
	pairs, _ := fingerprint.Dedupe(windows)
	if err := j.Commit("fingerprint", stageHashes["fingerprint"], nil); err != nil {
		return nil, err
	}

	pkg, err := buildPackage(director, stageTrack, primaryPCM, cutPlan.Segments, assignments, j, stageHashes)
	if err != nil {
		return nil, err
	}
	if req.Interrupt {
		return &Result{Code: ExitInterrupt, ReusedNodes: reusedAtStart}, ErrSimulatedInterrupt
	}

	outputSHA, err := manifest.HashOutputs(req.OutDir)
	if err != nil {
		return nil, err
	}
	maxSkew := cutPlan.MaxSkewNs
	m := &manifest.Manifest{
		Case: req.Case, Seed: req.Seed, Metrics: manifest.Metrics{
			TargetBPS: TargetBPS, ActualBPS: pkg.ActualBPS, BitrateDeltaPct: pkg.BitrateDelta,
			RMSDBFS: primaryMetrics.RMSDBFS, PeakDBFS: primaryMetrics.PeakDBFS,
			ClippedSamples: primaryMetrics.Clipped, SegmentCount: len(cutPlan.Segments),
			DedupeCount: len(pairs), ManifestMaxSkewNs: maxSkew,
			ReusedNodes: len(reusedAtStart), OutputSHA256: outputSHA,
		},
		Segments: pkg.Segments, Events: aligned, Assignments: assignments,
		Duplicates: pairs, ColorDecision: &colorDecision, Errors: allErrs,
	}
	if _, err := manifest.Write(req.OutDir, m); err != nil {
		return nil, err
	}
	if err := WriteMetrics(req.OutDir, m); err != nil {
		return nil, err
	}
	if err := j.Commit("manifest", stageHashes["manifest"], map[string]string{"manifest.json": outputSHA}); err != nil {
		return nil, err
	}
	m.Metrics.ReusedNodes = len(reusedAtStart)
	return &Result{Code: 0, ReusedNodes: reusedAtStart, OutputSHA256: outputSHA, Manifest: m}, nil
}

var ErrSimulatedInterrupt = errors.New("simulated recoverable interruption")

func colorToGraph(c ingest.ColorInfo, trackID int) graph.ColorDescription {
	return graph.ColorDescription{
		TrackID: trackID, Primaries: c.Primaries, Transfer: c.Transfer,
		Matrix: c.Matrix, FullRange: c.FullRange, HasMetadata: c.HasMetadata,
	}
}
