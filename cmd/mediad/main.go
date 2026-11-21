// Command mediad is the terminal entry point for the mediad processing
// library. It demonstrates the pipeline end to end and prints the acceptance
// fields; all real work lives in the internal packages.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/pipeline"
	"mediad/internal/report"
	"mediad/internal/txn"
)

// Exit codes are part of the command's contract.
const (
	exitOK        = 0
	exitFailure   = 1
	exitInvalid   = 2
	exitRecovery  = 3
	exitThreshold = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitInvalid
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "mediad: unknown command %q\n", args[0])
		usage(stderr)
		return exitInvalid
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mediad - deterministic media timeline verifier

usage:
  mediad demo  [flags]     run the full pipeline and print acceptance fields
  mediad probe <file>      print container metadata for a generated sample
  mediad help              show this message

demo flags:
  --seed int               random seed for fixtures and payloads (default 17)
  --faults list            comma list of duplicate,reorder,burst-loss,corrupt-log
  --bandwidth-mbps list    comma list of bandwidth values in Mbps
  --interrupt-after stage  simulate a crash after this stage
  --resume                 reuse checksum-verified stages after a crash
  --dir path               working directory (default: temporary)
  --seeks int              index probes per track (default 11)
  --max-burst int          longest recoverable packet gap (default 8)
  --json                   print only the machine-readable summary

exit codes:
  0 verified   2 invalid input or metadata   3 recovery or commit failed
  4 quantitative gate failed
`)
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 17, "random seed")
	faults := fs.String("faults", "", "comma list: duplicate,reorder,burst-loss,corrupt-log")
	bw := fs.String("bandwidth-mbps", "", "comma list of bandwidths in Mbps")
	interrupt := fs.String("interrupt-after", "", "stage after which to simulate a crash")
	resume := fs.Bool("resume", false, "reuse checksum-verified stages after a crash")
	dir := fs.String("dir", "", "working directory")
	seeks := fs.Int("seeks", 11, "index probes per track")
	maxBurst := fs.Int("max-burst", 8, "longest recoverable packet gap")
	asJSON := fs.Bool("json", false, "print only the JSON summary")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}

	faultList, err := parseFaults(*faults)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitInvalid
	}
	bandwidths, err := parseFloats(*bw)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: --bandwidth-mbps: %v\n", err)
		return exitInvalid
	}
	var stage txn.Stage
	if *interrupt != "" {
		stage = txn.Stage(*interrupt)
		valid := false
		for _, s := range txn.Order {
			if s == stage {
				valid = true
			}
		}
		if !valid {
			fmt.Fprintf(stderr, "mediad: --interrupt-after: unknown stage %q\n", *interrupt)
			return exitInvalid
		}
	}
	if *resume && stage == "" {
		// Resume without an explicit interruption point still recovers from
		// whatever the log contains, which is how a killed process restarts.
		if *dir == "" {
			fmt.Fprintf(stderr, "mediad: --resume without --dir has no log to resume from\n")
			return exitInvalid
		}
	}

	workDir := *dir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), fmt.Sprintf("mediad-demo-%d", *seed))
	}
	opt := pipeline.DemoOptions{
		Seed: *seed, Faults: faultList, BandwidthMbps: bandwidths,
		InterruptAfter: stage, Resume: *resume, Dir: workDir,
		SeeksPerTrack: *seeks, MaxBurst: *maxBurst,
	}
	res, err := pipeline.Run(opt)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		code := model.ExitCodeOf(err)
		if code == 0 {
			code = exitFailure
		}
		return code
	}

	// Verification gates: the manifest's alignment and bitrate thresholds,
	// then quality and index mismatches.
	gateErr := res.Manifest.Gate(1.0, 5.0)
	if gateErr == nil {
		gateErr = report.Gate(res.Summary, 30)
	}

	if *asJSON {
		data, err := res.Summary.JSON()
		if err != nil {
			fmt.Fprintf(stderr, "mediad: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "%s\n", data)
	} else {
		printReport(stdout, res)
	}
	if gateErr != nil {
		fmt.Fprintf(stderr, "mediad: verification failed: %v\n", gateErr)
		if gateErrKO(gateErr) {
			return exitThreshold
		}
		return model.ExitCodeOf(gateErr)
	}
	return exitOK
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func gateErrKO(err error) bool {
	return model.KindOf(err) == model.KindThreshold
}

func runProbe(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "mediad: probe takes exactly one file")
		return exitInvalid
	}
	_ = stdout
	_ = stderr
	fmt.Fprintln(stderr, "mediad: probe is available through the demo pipeline's checkpoints")
	return exitInvalid
}

func parseFaults(s string) ([]fixture.FaultKind, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []fixture.FaultKind
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k := fixture.FaultKind(part)
		switch k {
		case fixture.FaultDuplicate, fixture.FaultReorder, fixture.FaultBurstLoss, fixture.FaultCorruptLog:
			out = append(out, k)
		default:
			return nil, fmt.Errorf("unknown fault %q (want duplicate, reorder, burst-loss or corrupt-log)", part)
		}
	}
	return out, nil
}

func parseFloats(s string) ([]float64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("%q is not a positive number", part)
		}
		out = append(out, v)
	}
	return out, nil
}

func printReport(w io.Writer, res pipeline.Result) {
	s := res.Summary
	fmt.Fprintln(w, "mediad demo report")
	fmt.Fprintln(w, strings.Repeat("-", 46))
	fmt.Fprintf(w, "%-28s %s\n", "seed", s.Seed)
	if len(res.ReusedStages) > 0 {
		fmt.Fprintf(w, "%-28s %s\n", "reused_stages", strings.Join(res.ReusedStages, ","))
	}
	fmt.Fprintf(w, "%-28s %s\n", "rollback_stage", orNone(s.RollbackStage))
	fmt.Fprintf(w, "%-28s %s\n", "resume_stage", orNone(s.ResumeStage))
	if s.InjectedFaults > 0 {
		fmt.Fprintf(w, "%-28s %d\n", "injected_faults", s.InjectedFaults)
	}
	fmt.Fprintln(w, strings.Repeat("-", 46))
	fmt.Fprintf(w, "%-28s %.3f\n", "target_bitrate_kbps", s.TargetBitrateKbps)
	fmt.Fprintf(w, "%-28s %.3f\n", "actual_bitrate_kbps", s.ActualBitrateKbps)
	fmt.Fprintf(w, "%-28s %+.3f\n", "bitrate_delta_pct", s.BitrateDeltaPct)
	fmt.Fprintf(w, "%-28s %.2f\n", "quality_psnr_db", s.QualityPSNRdB)
	fmt.Fprintf(w, "%-28s %d\n", "recovered_packets", s.RecoveredPackets)
	fmt.Fprintf(w, "%-28s %d\n", "unrecoverable_packets", s.UnrecoverablePackets)
	fmt.Fprintf(w, "%-28s %d\n", "duplicate_packets", s.Duplicates)
	fmt.Fprintf(w, "%-28s %d\n", "reordered_packets", s.Reordered)
	fmt.Fprintf(w, "%-28s %.3f\n", "manifest_alignment_ms", s.ManifestAlignmentMS)
	fmt.Fprintf(w, "%-28s %d\n", "index_seek_mismatches", s.IndexSeekMismatches)
	fmt.Fprintf(w, "%-28s %s\n", "manifest_sha256", s.ManifestSHA256)
	if s.RepairWraps+s.RepairDuplicates+s.RepairAdjusted > 0 {
		fmt.Fprintf(w, "%-28s wraps=%d duplicates=%d adjusted=%d\n", "timestamp_repairs",
			s.RepairWraps, s.RepairDuplicates, s.RepairAdjusted)
	}
	if len(s.TrackBitrates) > 0 {
		fmt.Fprintln(w, strings.Repeat("-", 46))
		fmt.Fprintf(w, "%-6s %12s %12s %9s\n", "track", "target_kbps", "actual_kbps", "delta_pct")
		for _, t := range s.TrackBitrates {
			fmt.Fprintf(w, "%-6d %12.2f %12.2f %+9.2f\n", t.TrackID, t.TargetBitrateKbps, t.ActualBitrateKbps, t.DeltaPct)
		}
	}
	if len(res.Notes) > 0 {
		fmt.Fprintln(w, strings.Repeat("-", 46))
		for _, n := range res.Notes {
			fmt.Fprintf(w, "note: %s\n", n)
		}
	}
}
