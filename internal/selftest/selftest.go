// Package selftest orchestrates the end-to-end demonstration scenarios the
// avcore binary must prove on demand: a normal multi-rendition run, a run fed
// corrupted captions, a time-code wrap/negative-offset/drop-frame check and
// an interrupt-then-recovery run whose manifest digest must match a clean
// full rerun. It only depends on the same public packages the CLI uses.
package selftest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"avcore/internal/adapter"
	"avcore/internal/audit"
	"avcore/internal/caption"
	"avcore/internal/clock"
	"avcore/internal/demo"
	"avcore/internal/pipeline"
)

// Params are the parsed selftest flags.
type Params struct {
	OutDir       string
	Seed         int
	DurationS    int
	SegmentS     int
	Bitrates     []int
}

// Row is one line of the final report.
type Row struct {
	Case               string
	TargetBPS          int
	ActualBPS          int
	BitrateDeltaPct    float64
	PSNRMeanDB         float64
	SSIMMean           float64
	MaxAlignmentMS     float64
	BadRecords         int
	ReusedSegments     int
	ManifestSHAEqual   bool
}

// Check is a named boolean gate (time-code conversions and hard thresholds).
type Check struct {
	Name string
	OK   bool
	Detail string
}

// Report aggregates everything printed to the terminal.
type Report struct {
	Rows   []Row
	Checks []Check
}

// Gates encode the acceptance thresholds.
const (
	MaxBitrateDeltaPct = 15.0
	MinPSNRDB          = 28.0
	MinSSIM            = 0.90
	MaxAlignmentMS     = 33.4
)

// Run executes every scenario and returns the assembled report. The boolean
// is the overall pass/fail across all hard gates.
func Run(ctx context.Context, p Params) (*Report, bool, error) {
	rep := &Report{}
	rate := clock.Rate30
	fixtureDir := filepath.Join(p.OutDir, "fixtures")
	paths, err := demo.Generate(demo.Spec{
		Dir: fixtureDir, Seed: p.Seed, DurationS: p.DurationS,
		SegmentS: p.SegmentS, Rate: rate,
	})
	if err != nil {
		return nil, false, err
	}

	// Scenario 1: time-code wrap / negative offset / drop-frame conversions.
	rep.Checks = append(rep.Checks, checkTimecodes(paths.TimecodeCSV))

	// Scenario 2: normal run with valid captions.
	normalJob := filepath.Join(p.OutDir, "job_normal")
	normalCaps := parseSRT(paths.SRT)
	normalRes, err := runOnce(ctx, p, normalJob, paths.Source, normalCaps, rate, 0)
	if err != nil {
		return nil, false, err
	}
	rep.appendVariantRows("normal", normalRes, len(normalCaps.BadRecords))
	rep.Checks = append(rep.Checks, thresholdChecks("normal", rep.rowsFor("normal"))...)

	// Scenario 3: corrupted captions are isolated, the run still completes.
	badJob := filepath.Join(p.OutDir, "job_bad_captions")
	badCaps := parseSRT(paths.BadSRT)
	badRes, err := runOnce(ctx, p, badJob, paths.Source, badCaps, rate, 0)
	if err != nil {
		return nil, false, err
	}
	rep.appendVariantRows("corrupt_captions", badRes, len(badCaps.BadRecords))
	rep.Checks = append(rep.Checks, Check{
		Name: "corrupt_captions_isolated", OK: len(badCaps.BadRecords) > 0,
		Detail: fmt.Sprintf("bad_records=%d", len(badCaps.BadRecords)),
	})

	// Scenario 4: interrupt after half the cuts, then recover; compare the
	// recovered manifest digest against a clean full rerun.
	interruptAfter := len(normalRes.Journal.Captions) / 2
	if interruptAfter < 1 {
		interruptAfter = 1
	}
	recJob := filepath.Join(p.OutDir, "job_recovery")
	primeCfg := baseConfig(p, recJob, paths.Source, rate, interruptAfter)
	eng, err := pipeline.NewEngine()
	if err != nil {
		return nil, false, err
	}
	primeLog, _ := audit.NewLogger(filepath.Join(p.OutDir, "prime.jsonl"))
	_, ierr := eng.Run(ctx, primeCfg, normalCaps, primeLog)
	primeLog.Close()
	if !isInterrupt(ierr) {
		return nil, false, fmt.Errorf("expected injected interrupt, got %v", ierr)
	}
	recoverCfg := baseConfig(p, recJob, paths.Source, rate, 0)
	recLog, _ := audit.NewLogger(filepath.Join(p.OutDir, "recover.jsonl"))
	recRes, err := eng.Run(ctx, recoverCfg, normalCaps, recLog)
	recLog.Close()
	if err != nil {
		return nil, false, err
	}
	cleanJob := filepath.Join(p.OutDir, "job_clean")
	cleanRes, err := runOnce(ctx, p, cleanJob, paths.Source, normalCaps, rate, 0)
	if err != nil {
		return nil, false, err
	}
	shaEqual := recRes.ManifestSHA == cleanRes.ManifestSHA && recRes.ManifestSHA != ""
	rep.appendVariantRows("interrupt_recovery", recRes, 0)
	// Annotate the recovery rows with the digest gate and reuse count.
	for i := range rep.Rows {
		if rep.Rows[i].Case == "interrupt_recovery" {
			rep.Rows[i].ManifestSHAEqual = shaEqual
			rep.Rows[i].ReusedSegments = recRes.Summary.ReusedSegments
		}
	}
	rep.Checks = append(rep.Checks,
		Check{Name: "recovery_manifest_sha256_equal", OK: shaEqual,
			Detail: recRes.ManifestSHA[:12]},
		Check{Name: "recovery_reused_segments", OK: recRes.Summary.ReusedSegments > 0,
			Detail: fmt.Sprintf("reused=%d", recRes.Summary.ReusedSegments)},
	)

	pass := true
	for _, c := range rep.Checks {
		if !c.OK {
			pass = false
		}
	}
	return rep, pass, nil
}

func runOnce(ctx context.Context, p Params, jobDir, source string,
	caps caption.Result, rate clock.Rate, interrupt int) (*pipeline.Result, error) {
	eng, err := pipeline.NewEngine()
	if err != nil {
		return nil, err
	}
	cfg := baseConfig(p, jobDir, source, rate, interrupt)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return nil, err
	}
	logger, err := audit.NewLogger(filepath.Join(jobDir, "audit.jsonl"))
	if err != nil {
		return nil, err
	}
	defer logger.Close()
	return eng.Run(ctx, cfg, caps, logger)
}

func baseConfig(p Params, jobDir, source string, rate clock.Rate, interrupt int) pipeline.Config {
	rungs := make([]pipeline.RenditionConfig, len(p.Bitrates))
	for i, bps := range p.Bitrates {
		// Ladder: the lowest rung uses the smaller frame, higher rungs scale up.
		width, height := 320, 240
		if i > 0 {
			width, height = 640, 480
		}
		rungs[i] = pipeline.RenditionConfig{
			Name: fmt.Sprintf("r%d", i), TargetBPS: bps, Width: width, Height: height,
		}
	}
	return pipeline.Config{
		Source: source, JobDir: jobDir,
		SegmentTicks: clock.Millis(int64(p.SegmentS) * 1000),
		FrameRate:    rate, InterruptAfterCuts: interrupt,
		Renditions:   rungs,
	}
}

func (r *Report) appendVariantRows(caseName string, res *pipeline.Result, bad int) {
	for _, v := range res.Summary.Variants {
		r.Rows = append(r.Rows, Row{
			Case: caseName, TargetBPS: v.TargetBPS, ActualBPS: v.ActualBPS,
			BitrateDeltaPct: v.BitrateDeltaPct, PSNRMeanDB: v.PSNRMeanDB,
			SSIMMean: v.SSIMMean, MaxAlignmentMS: res.Summary.MaxAlignmentMS,
			BadRecords: bad, ReusedSegments: res.Summary.ReusedSegments,
			ManifestSHAEqual: res.Summary.ManifestSHAEqual,
		})
	}
}

func (r *Report) rowsFor(caseName string) []Row {
	var out []Row
	for _, row := range r.Rows {
		if row.Case == caseName {
			out = append(out, row)
		}
	}
	return out
}

func thresholdChecks(caseName string, rows []Row) []Check {
	var checks []Check
	for _, row := range rows {
		prefix := fmt.Sprintf("%s@%d", caseName, row.TargetBPS)
		checks = append(checks,
			Check{Name: prefix + "_bitrate", OK: abs(row.BitrateDeltaPct) <= MaxBitrateDeltaPct,
				Detail: fmt.Sprintf("delta=%.2f%%", row.BitrateDeltaPct)},
			Check{Name: prefix + "_psnr", OK: row.PSNRMeanDB >= MinPSNRDB,
				Detail: fmt.Sprintf("psnr=%.2f", row.PSNRMeanDB)},
			Check{Name: prefix + "_ssim", OK: row.SSIMMean >= MinSSIM,
				Detail: fmt.Sprintf("ssim=%.4f", row.SSIMMean)},
			Check{Name: prefix + "_alignment", OK: row.MaxAlignmentMS <= MaxAlignmentMS,
				Detail: fmt.Sprintf("align=%.3fms", row.MaxAlignmentMS)},
		)
	}
	return checks
}

func parseSRT(path string) caption.Result {
	f, err := os.Open(path)
	if err != nil {
		return caption.Result{Fatal: adapter.NewError(adapter.CodePathRejected, err.Error(),
			adapter.Source{File: path})}
	}
	defer f.Close()
	return caption.ParseSRT(f, caption.MaxCaptionBytes)
}

func isInterrupt(err error) bool {
	if ae, ok := err.(*adapter.Error); ok {
		return ae.Code == adapter.CodeInterrupted
	}
	return false
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
