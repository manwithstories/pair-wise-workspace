// Command bcast runs broadcast batch verification, repair, indexing and
// transactional commit/recovery demonstrations.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"bcast/internal/fixture"
	"bcast/internal/media"
	"bcast/internal/metrics"
	"bcast/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "resume":
		return runResume(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bcast demo --case NAME --out DIR [--fail-after STAGE] [--expect-error]")
	fmt.Fprintln(os.Stderr, "       bcast resume --out DIR")
	fmt.Fprintln(os.Stderr, "cases: clean truncated duplicate drift-wrap-vfr commit-interrupt")
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	caseName := fs.String("case", "", "generated sample case")
	out := fs.String("out", "", "batch output directory")
	failAfter := fs.String("fail-after", "", "inject exit 75 after scan|repair|index|metrics")
	expectError := fs.Bool("expect-error", false, "expected corrupt fixture: print result and exit 0")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *caseName == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "demo requires --case and --out")
		return 1
	}
	valid := map[string]bool{
		"clean": true, "truncated": true, "duplicate": true,
		"drift-wrap-vfr": true, "commit-interrupt": true,
	}
	if !valid[*caseName] {
		fmt.Fprintf(os.Stderr, "unknown case %q\n", *caseName)
		return 1
	}

	source := filepath.Join(*out, "input")
	cfg, err := fixture.Generate(*caseName, source)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return media.ExitCodeOf(err)
	}
	report, err := pipeline.Run(pipeline.Options{
		Out:        *out,
		SourceDir:  source,
		FailAfter:  *failAfter,
		TargetKBPS: cfg.TargetKBPS,
	})
	if err != nil {
		code := media.ExitCodeOf(err)
		if code == media.ExitCorrupt && *expectError {
			if report != nil {
				printReport(report)
			}
			return 0
		}
		if report != nil {
			printReport(report)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return code
	}
	printReport(report)
	return 0
}

func runResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	out := fs.String("out", "", "batch output directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "resume requires --out")
		return 1
	}
	report, err := pipeline.Resume(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return media.ExitCodeOf(err)
	}
	printReport(report)
	return 0
}

func printReport(r *metrics.Report) {
	if r == nil {
		return
	}
	fmt.Printf("status=%s\n", r.Status)
	fmt.Printf("target_kbps=%d\n", r.TargetKBPS)
	fmt.Printf("actual_kbps=%.3f\n", r.ActualKBPS)
	fmt.Printf("bitrate_delta_pct=%.6f\n", r.BitrateDeltaPct)
	fmt.Printf("sample_count=%d\n", r.SampleCount)
	fmt.Printf("unique_fragment_count=%d\n", r.UniqueFragmentCount)
	fmt.Printf("sample_dur_p50_ms=%.3f\n", r.SampleDurP50MS)
	fmt.Printf("sample_dur_p95_ms=%.3f\n", r.SampleDurP95MS)
	fmt.Printf("sample_dur_p99_ms=%.3f\n", r.SampleDurP99MS)
	fmt.Printf("sync_point_count=%d\n", r.SyncPointCount)
	fmt.Printf("sync_interval_p50_ms=%.3f\n", r.SyncIntervalP50MS)
	fmt.Printf("sync_interval_p95_ms=%.3f\n", r.SyncIntervalP95MS)
	fmt.Printf("manifest_skew_ms=%.3f\n", r.ManifestSkewMS)
	fmt.Printf("seek_ok=%d/%d\n", r.SeekOK, r.SeekTotal)
	fmt.Printf("missing_count=%d\n", r.MissingCount)
	fmt.Printf("duplicate_count=%d\n", r.DuplicateCount)
	fmt.Printf("out_of_order_count=%d\n", r.OutOfOrderCount)
	fmt.Printf("isolated_count=%d\n", r.IsolatedCount)
	fmt.Printf("clock_drift_ppm=%.2f\n", r.ClockDriftPPM)
	if r.FirstErrorFragment != 0 {
		fmt.Printf("first_error_fragment=%d\n", r.FirstErrorFragment)
	}
	if r.FirstErrorByte != 0 {
		fmt.Printf("first_error_byte=%d\n", r.FirstErrorByte)
	}
	fmt.Printf("rollback=%d\n", r.Rollback)
	fmt.Printf("recovered=%d\n", r.Recovered)
	fmt.Printf("committed=%d\n", r.Committed)
	if r.Generation != "" {
		fmt.Printf("generation=%s\n", r.Generation)
	}
	if r.SHA256 != "" {
		fmt.Printf("sha256=%s\n", r.SHA256)
	}
}
