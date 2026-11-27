// Command fastpost is the terminal entry point for the subway-drill
// multi-camera remuxing pipeline. It generates deterministic fixtures,
// aligns multi-bitrate segments and publishes HLS manifests with
// checkpoint-based recovery.
package main

import (
	"flag"
	"fmt"
	"os"

	"fastpost/internal/model"
	"fastpost/internal/pipeline"
)

// Exit codes follow the delivery contract.
const (
	exitOK               = 0
	exitUsage            = 1
	exitUnrecoverable    = 2
	exitChecksumMismatch = 3
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "version":
		fmt.Println("fastpost 1.0.0")
		os.Exit(exitOK)
	default:
		usage()
		os.Exit(exitUsage)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr,
		"usage: fastpost demo --seed N --segments N [--fault kind:offset] [--interrupt-after N] [--resume] --out DIR")
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Uint64("seed", 0, "deterministic PRNG seed")
	segments := fs.Int("segments", 0, "number of segments per variant")
	fault := fs.String("fault", "", "injected fault, e.g. sei-truncate:4096")
	interruptAfter := fs.Int("interrupt-after", 0, "simulate an interrupt after N segments")
	resume := fs.Bool("resume", false, "resume from a prior checkpoint when present")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *segments <= 0 || *out == "" {
		fmt.Fprintln(os.Stderr, "demo requires --segments > 0 and --out")
		return exitUsage
	}

	cfg := pipeline.Config{
		Seed: *seed, Segments: *segments, Fault: *fault,
		InterruptAfter: *interruptAfter, Resume: *resume, OutDir: *out,
	}
	report, err := pipeline.Run(cfg)
	// A controlled interruption writes a partial checkpoint. With --resume
	// the same invocation immediately runs the completion phase against that
	// checkpoint, modeling crash recovery in a single terminal command.
	if err != nil && *resume && pipeline.IsInterrupted(err) {
		report, err = pipeline.Run(cfg)
	}
	// A report may accompany an error (for example an injected input fault);
	// print the summary first so the operator still gets the full metrics.
	if report != nil {
		emitReport(report)
	}
	if err != nil {
		return emitError(err)
	}
	if report != nil && hasChecksumFault(report.Faults) {
		return exitChecksumMismatch
	}
	return exitOK
}

func emitReport(r *pipeline.Report) {
	fmt.Printf("segments=%d\n", r.Segments)
	fmt.Printf("variants=%d\n", r.Variants)
	fmt.Printf("aligned=%d\n", r.Aligned)
	fmt.Printf("skew_ticks=%d\n", r.SkewTicks)
	fmt.Printf("fault_expected=%d\n", r.FaultExpected)
	fmt.Printf("fault_found=%d\n", r.FaultFound)
	fmt.Printf("resume_ms=%d\n", r.ResumeMS)
	fmt.Printf("sha256=%s\n", r.Checksum)
	for _, f := range r.Faults {
		fmt.Fprintln(os.Stderr, f.String())
	}
}

func emitError(err error) int {
	// Fault lines are already emitted by emitReport when a report is present;
	// here we only translate the error class into the contracted exit code.
	if in, ok := err.(*model.InputError); ok {
		for _, f := range in.Faults {
			if f.Kind == model.FaultChecksum {
				return exitChecksumMismatch
			}
		}
		return exitUnrecoverable
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return exitUnrecoverable
}

func hasChecksumFault(faults []model.Fault) bool {
	for _, f := range faults {
		if f.Kind == model.FaultChecksum {
			return true
		}
	}
	return false
}
