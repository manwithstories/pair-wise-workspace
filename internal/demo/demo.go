// Package demo runs the end-to-end acceptance scenario entirely from
// generated local fixtures: a healthy transmission, fault-injected damage
// location and an interrupted/resumed run cross-checked against an
// independent full run.
package demo

import (
	"fmt"
	"os"
	"path/filepath"

	"mediaroute/internal/engine"
	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
	"mediaroute/internal/probe"
)

// Options mirrors the demo command line flags.
type Options struct {
	Seed           int64
	DropBurst      int
	DropBurstAt    int
	Duplicate      int
	ReorderWindow  int
	BandwidthMbps  []float64
	InterruptAfter int
	Resume         bool
}

// PhaseResult is one labelled stage of the demo.
type PhaseResult struct {
	Name    string
	Summary *engine.Summary
}

// Result collects all demo phases and the cross-check verdict.
type Result struct {
	WorkDir string
	// ExtraDirs holds additional scratch directories (e.g. the independent
	// full run) so callers can clean everything up.
	ExtraDirs []string
	Healthy   *engine.Summary
	Damaged   *engine.Summary
	Resume    *engine.Summary
	Full      *engine.Summary
	Phases    []PhaseResult
	// ExpectedUncommitted is the sample count remaining after the
	// simulated interruption.
	ExpectedUncommitted int
}

// Run executes every demo phase and validates the acceptance invariants.
func Run(opt Options) (*Result, error) {
	dir, err := os.MkdirTemp("", "mediaroute-demo-*")
	if err != nil {
		return nil, fault.New(fault.CatIO, "workdir: %v", err)
	}
	res := &Result{WorkDir: dir}
	healthySpec := fixture.DefaultSpec(opt.Seed)
	input, err := writeInput(dir, healthySpec)
	if err != nil {
		return nil, err
	}
	indexDir := filepath.Join(dir, "index")

	// Phase 1: healthy transmission.
	healthySR, err := engine.Scan(engine.ScanConfig{
		InputPath: input, IndexDir: indexDir, Params: index.DefaultParams(),
	})
	if err != nil {
		return nil, err
	}
	healthyST, err := engine.Stream(engine.StreamConfig{
		Spec: healthySpec, Report: healthySR.Report,
		OutputPath:    filepath.Join(dir, "healthy.out"),
		Checkpoint:    filepath.Join(dir, "healthy.cp"),
		GapTimeoutNs:  50_000_000,
		BandwidthMbps: opt.BandwidthMbps,
	})
	if err != nil {
		return nil, err
	}
	res.Healthy = engine.BuildSummary(healthySR, healthyST, -1)
	res.Phases = append(res.Phases, PhaseResult{Name: "healthy", Summary: res.Healthy})
	if res.Healthy.Damage.OffsetErrorSamples != 0 {
		return nil, fault.New(fault.CatSampleDamage,
			"healthy run reports offset error %d",
			res.Healthy.Damage.OffsetErrorSamples)
	}

	// Phase 2: fault-injected damage location.
	dmgSpec := healthySpec
	dmgSpec.DropBurstN = opt.DropBurst
	dmgSpec.DropBurstAt = opt.DropBurstAt
	if dmgSpec.DropBurstAt < 0 && opt.DropBurst > 0 {
		dmgSpec.DropBurstAt = 30
	}
	dmgSpec.DuplicateN = opt.Duplicate
	dmgSpec.ReorderWin = opt.ReorderWindow
	dmgST, err := engine.Stream(engine.StreamConfig{
		Spec: dmgSpec, Report: healthySR.Report,
		OutputPath:    filepath.Join(dir, "damaged.out"),
		Checkpoint:    filepath.Join(dir, "damaged.cp"),
		GapTimeoutNs:  50_000_000,
		BandwidthMbps: opt.BandwidthMbps,
	})
	if err != nil {
		return nil, err
	}
	expectedFirst := int64(-1)
	if len(dmgST.Gaps) > 0 {
		expectedFirst = healthySR.Report.Tracks[0].
			FindSampleAt(dmgST.Gaps[0].TimeRange.StartNanos)
	}
	res.Damaged = engine.BuildSummary(healthySR, dmgST, expectedFirst)
	res.Phases = append(res.Phases, PhaseResult{Name: "damage", Summary: res.Damaged})
	if res.Damaged.Damage.OffsetErrorSamples > 1 {
		return nil, fault.New(fault.CatSampleDamage,
			"damage offset error %d exceeds one sample",
			res.Damaged.Damage.OffsetErrorSamples)
	}

	// Phase 3: interruption and resume.
	if opt.InterruptAfter > 0 {
		if err := runResumePhase(opt, healthySpec, healthySR.Report, dir, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func runResumePhase(opt Options, spec fixture.Spec, report *probe.ContainerReport,
	dir string, res *Result) error {
	outPath := filepath.Join(dir, "resume.out")
	cpPath := filepath.Join(dir, "resume.cp")
	common := engine.StreamConfig{
		Spec: spec, Report: report,
		OutputPath:    outPath,
		Checkpoint:    cpPath,
		GapTimeoutNs:  50_000_000,
		BandwidthMbps: opt.BandwidthMbps,
	}
	// Interrupted first run.
	first := common
	first.InterruptAfter = opt.InterruptAfter
	st1, err := engine.Stream(first)
	if err != nil {
		return err
	}
	if !st1.Interrupted {
		return fault.New(fault.CatCheckpointMismatch,
			"interruption did not trigger at sample %d", opt.InterruptAfter)
	}
	res.ExpectedUncommitted = spec.SampleCount - st1.Committed
	if !opt.Resume {
		return nil
	}
	// Resumed run; only its own elapsed time is counted.
	st2, err := engine.Stream(common)
	if err != nil {
		return err
	}
	sr := &engine.ScanResult{Report: report, Reused: true}
	res.Resume = engine.BuildSummary(sr, st2, -1)
	res.Phases = append(res.Phases, PhaseResult{Name: "resume", Summary: res.Resume})

	// Phase 4: independent full run on a freshly generated asset.
	fullDir, err := os.MkdirTemp("", "mediaroute-full-*")
	if err != nil {
		return err
	}
	res.ExtraDirs = append(res.ExtraDirs, fullDir)
	fullInput, err := writeInput(fullDir, spec)
	if err != nil {
		return err
	}
	fullSR, err := engine.Scan(engine.ScanConfig{
		InputPath: fullInput, IndexDir: filepath.Join(fullDir, "idx"),
		Params: index.DefaultParams(),
	})
	if err != nil {
		return err
	}
	fullST, err := engine.Stream(engine.StreamConfig{
		Spec: spec, Report: fullSR.Report,
		OutputPath:    filepath.Join(fullDir, "full.out"),
		Checkpoint:    filepath.Join(fullDir, "full.cp"),
		GapTimeoutNs:  50_000_000,
		BandwidthMbps: opt.BandwidthMbps,
	})
	if err != nil {
		return err
	}
	res.Full = engine.BuildSummary(fullSR, fullST, -1)
	res.Phases = append(res.Phases, PhaseResult{Name: "full", Summary: res.Full})

	// Invariants.
	if st2.ReplayedSamples > res.ExpectedUncommitted {
		return fault.New(fault.CatCheckpointMismatch,
			"replayed %d exceeds uncommitted %d",
			st2.ReplayedSamples, res.ExpectedUncommitted)
	}
	if st2.ElapsedNanos/1_000_000 >= 5000 {
		return fault.New(fault.CatCheckpointMismatch,
			"resume elapsed %dms exceeds 5000ms budget",
			st2.ElapsedNanos/1_000_000)
	}
	if st2.OutputSHA256 != fullST.OutputSHA256 {
		return fault.New(fault.CatCheckpointMismatch,
			"resumed output %s differs from full run %s",
			st2.OutputSHA256, fullST.OutputSHA256)
	}
	return nil
}

func writeInput(dir string, spec fixture.Spec) (string, error) {
	raw, err := spec.BuildMP4()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "asset.mp4")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return "", fault.New(fault.CatIO, "write fixture: %v", err)
	}
	return p, nil
}

// Print emits the acceptance metrics in the exact dotted-key form required
// by the demo contract, followed by the full JSON summary.
func (r *Result) Print() {
	if r.Healthy != nil {
		fmt.Printf("scan.samples %d\n", r.Healthy.Scan.Samples)
		fmt.Printf("keyframe.interval_p95_ms %d\n", r.Healthy.Keyframe.IntervalP95MS)
	}
	if r.Damaged != nil {
		fmt.Printf("damage.offset_error_samples %d\n", r.Damaged.Damage.OffsetErrorSamples)
	}
	if r.Resume != nil {
		fmt.Printf("resume.elapsed_ms %d\n", r.Resume.Resume.ElapsedMS)
		fmt.Printf("resume.replayed_samples %d\n", r.Resume.Resume.ReplayedSamples)
	}
	if r.Full != nil {
		fmt.Printf("output.sha256 %s\n", r.Full.Output.SHA256)
	}
}
