// mediascan verifies local multi-bitrate DASH and HLS inputs: manifest
// round-trips, unified rational-second timeline mapping, fMP4 structure
// and keyframe analysis, per-segment hashing, resumable checkpoints and
// a deterministic provenance report. Terminal-only, no network access.
//
// Exit codes: 0 = completed, no damage; 1 = input/config error;
// 2 = report completed, damage found; 75 = resumable interruption.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediascan/internal/fixture"
	"mediascan/internal/index"
	"mediascan/internal/pipeline"
	"mediascan/internal/provenance"
)

const (
	exitOK        = 0
	exitError     = 1
	exitDamage    = 2
	exitInterrupt = 75
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitError
	}
	switch args[0] {
	case "fixture":
		return cmdFixture(args[1:])
	case "scan":
		return cmdScan(args[1:])
	default:
		usage()
		return exitError
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mediascan fixture --out DIR [--fault KIND]
  mediascan scan --input DIR --state DIR [--hashes] [--stop-after N] [--resume] [--concurrency N]

fault kinds: truncated-box, missing-segment, invalid-duration, tfdt-rollback, discontinuity-boundary
exit codes: 0 ok, 1 input/config error, 2 damage found, 75 resumable interruption`)
}

// rejectRemote refuses non-local inputs.
func rejectRemote(kind, p string) error {
	if strings.Contains(p, "://") || strings.HasPrefix(p, "//") {
		return fmt.Errorf("%s: remote address rejected: %q", kind, p)
	}
	return nil
}

func cmdFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	out := fs.String("out", "", "output directory (required)")
	faultStr := fs.String("fault", "", "fault to inject")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "fixture: --out is required")
		return exitError
	}
	if err := rejectRemote("fixture", *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	fault, err := fixture.ParseFault(*faultStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		return exitError
	}
	if err := fixture.Generate(*out, fault); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		return exitError
	}
	faultLabel := string(fault)
	if faultLabel == "" {
		faultLabel = "none"
	}
	fmt.Printf("fixture generated: out=%s fault=%s segments=%d\n", *out, faultLabel, fixture.TotalSegments)
	return exitOK
}

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	input := fs.String("input", "", "input directory (required)")
	state := fs.String("state", "", "state directory (required)")
	hashes := fs.Bool("hashes", false, "compute per-segment SHA-256")
	stopAfter := fs.Int("stop-after", 0, "interrupt after N newly processed segments (exit 75)")
	resume := fs.Bool("resume", false, "resume from checkpoint (checkpoints are always reused when digests match)")
	conc := fs.Int("concurrency", 4, "worker count (bounded to 16)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	_ = *resume
	if *input == "" || *state == "" {
		fmt.Fprintln(os.Stderr, "scan: --input and --state are required")
		return exitError
	}
	if err := rejectRemote("scan", *input); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	fi, err := os.Stat(*input)
	if err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "scan: input is not a directory: %s\n", *input)
		return exitError
	}
	if *stopAfter < 0 {
		fmt.Fprintln(os.Stderr, "scan: --stop-after must be >= 0")
		return exitError
	}

	cfg := index.Config{Hashes: *hashes}
	idx, rebuilt, err := index.LoadOrBuild(*state, *input, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		return exitError
	}
	fmt.Printf("index: segments=%d roundtrip=%d/%d rebuilt=%v timeline_errors=%d\n",
		len(idx.Segments), idx.RoundTripOK, idx.RoundTripTotal, rebuilt, len(idx.TimelineErrors))
	for _, r := range idx.TimelineErrors {
		fmt.Printf("timeline_error %s..%s reason=%s\n", r.Start, r.End, r.Reason)
	}

	out, err := pipeline.Run(idx, pipeline.Options{
		Root: *input, StateDir: *state, Hashes: *hashes,
		Concurrency: *conc, StopAfter: *stopAfter,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		return exitError
	}

	kfHits, hashed := 0, 0
	for _, s := range idx.Segments {
		res := out.Checkpoint.Results[s.ID]
		if res == nil {
			fmt.Printf("segment %s state=pending\n", s.ID)
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "segment %s state=%s time=%s..%s", s.ID, res.State, res.Start, res.End)
		if res.SHA256 != "" {
			fmt.Fprintf(&b, " sha256=%s", res.SHA256)
			hashed++
		}
		if len(res.Keyframes) > 0 {
			fmt.Fprintf(&b, " keyframes=%v gaps=%v", res.Keyframes, res.Gaps)
			kfHits++
		}
		for _, e := range res.Errors {
			fmt.Fprintf(&b, " error=%q", e)
		}
		fmt.Println(b.String())
	}
	total := len(idx.Segments)
	fmt.Printf("keyframe_hits=%d/%d\n", kfHits, total)
	fmt.Printf("hashed=%d/%d\n", hashed, total)
	fmt.Printf("damage_ranges=%d\n", len(out.Damage))
	for _, d := range out.Damage {
		fmt.Printf("damage %s..%s source=%s reason=%s\n", d.Start, d.End, d.Source, d.Reason)
	}
	fmt.Printf("new_processed=%d\n", out.NewProcessed)

	if out.Interrupted {
		fmt.Println("interrupted: checkpoint saved; resume with --resume (exit_code=75)")
		return exitInterrupt
	}

	rec := provenance.Build(idx, out.Checkpoint, out.Damage)
	if err := index.AtomicWriteJSON(filepath.Join(*state, "report.json"), rec); err != nil {
		fmt.Fprintln(os.Stderr, "scan: write report:", err)
		return exitError
	}
	fmt.Printf("report_sha256=%s\n", rec.SHA256())
	if len(out.Damage) > 0 || len(idx.TimelineErrors) > 0 {
		return exitDamage
	}
	return exitOK
}
