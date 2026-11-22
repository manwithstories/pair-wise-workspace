// Command archivecheck runs the archival recovery pipeline against generated
// Matroska samples.  It exposes three acceptance paths — normal, corrupt and
// resume — over a pure terminal interface and prints the metric bundle that
// the acceptance script checks.  Exit status: 0 verified, 2 damaged input but
// fully diagnosed, 3 transaction recovery or integrity failure.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"archivecheck/internal/fault"
	"archivecheck/internal/pipeline"
	"archivecheck/internal/txn"
)

const usage = `archivecheck - Matroska archive recovery checker

Usage:
  archivecheck demo   [flags]   normal, corrupt and resume paths with metrics
  archivecheck verify [flags]   re-run the demo path and check acceptance

Flags:
  --seed N              fixture seed (default 41)
  --loss-burst N        contiguous RTP burst loss length
  --reorder N           number of adjacent packet swaps
  --duplicate N         number of duplicated packets
  --bandwidth-mbps LIST comma-separated throughput samples, e.g. 2,0.4,4
  --frames N            frames per track (default 60)
  --interrupt-after S   abort after stage S (parse|repair|index|manifest)
  --resume              continue an interrupted run from its journal
  --memory-bytes N      transport buffer budget (default 1048576)
  --workdir PATH        working directory (default a fresh temp dir)
  --json                emit machine-readable JSON
`

type options struct {
	seed           int64
	lossBurst      int
	reorder        int
	duplicate      int
	bandwidth      string
	frames         int
	interruptAfter string
	resume         bool
	memoryBytes    int64
	workdir        string
	jsonOut        bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return fault.ExitDamaged
	}
	cmd := args[0]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return fault.ExitOK
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "archivecheck: unknown command %q\n\n%s", cmd, usage)
		return fault.ExitDamaged
	}
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("archivecheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&o.seed, "seed", 41, "fixture seed")
	fs.IntVar(&o.lossBurst, "loss-burst", 0, "RTP burst loss length")
	fs.IntVar(&o.reorder, "reorder", 0, "adjacent packet swaps")
	fs.IntVar(&o.duplicate, "duplicate", 0, "duplicated packets")
	fs.StringVar(&o.bandwidth, "bandwidth-mbps", "2", "comma-separated Mbps samples")
	fs.IntVar(&o.frames, "frames", 60, "frames per track")
	fs.StringVar(&o.interruptAfter, "interrupt-after", "", "stage to interrupt after")
	fs.BoolVar(&o.resume, "resume", false, "resume an interrupted run")
	fs.Int64Var(&o.memoryBytes, "memory-bytes", 1<<20, "transport buffer budget")
	fs.StringVar(&o.workdir, "workdir", "", "working directory")
	fs.BoolVar(&o.jsonOut, "json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	return o, nil
}

func bandwidth(o options) ([]float64, error) {
	var out []float64
	for _, part := range strings.Split(o.bandwidth, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(part, "%g", &v); err != nil {
			return nil, fmt.Errorf("invalid bandwidth sample %q", part)
		}
		if v <= 0 {
			return nil, fmt.Errorf("bandwidth sample %q must be positive", part)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no bandwidth samples")
	}
	return out, nil
}

// runDemo exercises normal, corrupt and resumable paths and prints metrics.
func runDemo(args []string) int {
	o, err := parseFlags(args)
	if err != nil {
		return fault.ExitDamaged
	}
	bw, err := bandwidth(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: %v\n", err)
		return fault.ExitDamaged
	}
	base := o.workdir
	if base == "" {
		tmp, err := os.MkdirTemp("", "archivecheck-demo-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "archivecheck: %v\n", err)
			return fault.ExitIntegrity
		}
		defer os.RemoveAll(tmp)
		base = tmp
	}

	cfg := pipeline.Config{
		Seed: o.seed, FrameCount: o.frames,
		LossBurst: o.lossBurst, Reorder: o.reorder, Duplicate: o.duplicate,
		BandwidthMbps: bw, MemoryBudget: o.memoryBytes,
	}

	// 1. Normal path.
	normal, err := pipeline.Run(withDir(cfg, base, "normal", false))
	if err != nil {
		return reportFailure("normal", err)
	}
	// 2. Corrupt path.
	corrupt, err := pipeline.Run(withDir(cfg, base, "corrupt", true))
	if err != nil {
		return reportFailure("corrupt", err)
	}
	// 3. Interrupted + resumed path.  The first run aborts after committing
	// the index stage (leaving an uncommitted manifest prepare plus a torn
	// journal tail); the second resumes from that journal.
	interruptCfg := withDir(cfg, base, "resume", false)
	interruptCfg.InterruptAfter = o.interruptAfter
	if interruptCfg.InterruptAfter == "" {
		interruptCfg.InterruptAfter = string(txn.StageIndex)
	}
	interruptCfg.CrashOnInterrupt = true
	interrupted, err := pipeline.Run(interruptCfg)
	if err != nil {
		return reportFailure("interrupted", err)
	}
	resumeCfg := withDir(cfg, base, "resume", false)
	resumeCfg.Resume = true
	resumed, err := pipeline.Run(resumeCfg)
	if err != nil {
		return reportFailure("resume", err)
	}

	for _, r := range []*pipeline.Report{normal, corrupt, interrupted, resumed} {
		if r.ExitCode == fault.ExitIntegrity {
			return fault.ExitIntegrity
		}
	}

	if o.jsonOut {
		return emitJSON(map[string]any{
			"normal": normal, "corrupt": corrupt,
			"interrupted": interrupted, "resumed": resumed,
		}, chooseExit(normal, corrupt, resumed))
	}
	printReport("normal", normal)
	printReport("corrupt", corrupt)
	printReport("interrupted", interrupted)
	printReport("resumed", resumed)
	printDeterminism(normal, resumed)
	return chooseExit(normal, corrupt, resumed)
}

// withDir builds a per-path config rooted in its own directory.
func withDir(cfg pipeline.Config, base, name string, corrupt bool) pipeline.Config {
	cfg.WorkingDir = base + "/" + name
	cfg.Corrupt = corrupt
	return cfg
}

// runVerify re-runs the normal path and enforces the acceptance thresholds.
func runVerify(args []string) int {
	o, err := parseFlags(args)
	if err != nil {
		return fault.ExitDamaged
	}
	bw, err := bandwidth(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archivecheck: %v\n", err)
		return fault.ExitDamaged
	}
	base := o.workdir
	if base == "" {
		tmp, err := os.MkdirTemp("", "archivecheck-verify-*")
		if err != nil {
			return fault.ExitIntegrity
		}
		defer os.RemoveAll(tmp)
		base = tmp
	}
	cfg := pipeline.Config{
		Seed: o.seed, FrameCount: o.frames,
		LossBurst: o.lossBurst, Reorder: o.reorder, Duplicate: o.duplicate,
		BandwidthMbps: bw, MemoryBudget: o.memoryBytes,
		WorkingDir: base + "/verify",
	}
	rep, err := pipeline.Run(cfg)
	if err != nil {
		return reportFailure("verify", err)
	}
	if o.jsonOut {
		return emitJSON(rep, chooseExit(rep))
	}
	printReport("verify", rep)
	problems := acceptance(rep)
	if len(problems) > 0 {
		fmt.Println("acceptance: FAIL")
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
		return fault.ExitDamaged
	}
	fmt.Println("acceptance: PASS")
	return rep.ExitCode
}

// acceptance lists every threshold violation.
func acceptance(rep *pipeline.Report) []string {
	var out []string
	s := rep.Stats
	if s.Bitrate.TargetBps > 0 {
		if pct := s.Bitrate.DeltaPct; pct > 5 || pct < -5 {
			out = append(out, fmt.Sprintf("bitrate delta %.2f%% exceeds 5%%", pct))
		}
	}
	if s.ManifestAlignMS > 1.0 {
		out = append(out, fmt.Sprintf("manifest alignment %.3f ms exceeds 1 ms", s.ManifestAlignMS))
	}
	if s.Quality.Samples == 0 {
		out = append(out, "quality sample count is zero")
	}
	if s.IndexSHA256 == "" {
		out = append(out, "index digest is empty")
	}
	return out
}

func chooseExit(reports ...*pipeline.Report) int {
	worst := fault.ExitOK
	for _, r := range reports {
		if r.ExitCode > worst {
			worst = r.ExitCode
		}
	}
	return worst
}

func reportFailure(path string, err error) int {
	fmt.Fprintf(os.Stderr, "archivecheck: %s failed: %v\n", path, err)
	return fault.ExitIntegrity
}

func emitJSON(v any, exit int) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fault.ExitIntegrity
	}
	return exit
}
