// Command mediasync batch-imports studio, field and proxy MXF/MP4 material into
// one media package. It is terminal-only: every result is printed as key=value
// lines on stdout, and the exit code carries the failure class.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediasync/internal/cli"
	"mediasync/internal/fixtures"
	"mediasync/internal/model"
	"mediasync/internal/pipeline"
)

// Exit codes agreed with the demo contract.
const (
	exitOK            = model.ExitOK
	exitParamError    = model.ExitParamError
	exitDataError     = model.ExitDataError
	exitUnrecoverable = model.ExitUnrecoverable
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return exitParamError
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return exitParamError
	}
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	f, err := cli.ParseDemo(args, stderr)
	if err != nil {
		fmt.Fprintf(stdout, "error_code=%s\n", model.ErrParamInvalid)
		return exitParamError
	}
	if err := f.Validate(); err != nil {
		fmt.Fprintf(stdout, "error_code=%s\nerror_message=%s\n", model.ErrParamInvalid, sanitize(err.Error()))
		return exitParamError
	}

	work, cleanup, err := prepareWorkDir(f.WorkDir)
	if err != nil {
		fmt.Fprintf(stdout, "error_code=%s\nerror_message=%s\n", model.ErrParamInvalid, sanitize(err.Error()))
		return exitParamError
	}
	defer cleanup()

	// Generate every fixture, including the damaged variants.
	if _, err := fixtures.Generate(filepath.Join(work, "input")); err != nil {
		fmt.Fprintf(stdout, "error_code=%s\nerror_message=%s\n", model.ErrUnrecoverable, sanitize(err.Error()))
		return exitUnrecoverable
	}
	// Regenerate the RTP stream with the flag-selected impairments.
	rtp, err := fixtures.BuildRTP(fixtures.RTPOptionsFromFlags(f.Seed, f.LossBurst, f.Duplicate, f.Reorder))
	if err != nil {
		fmt.Fprintf(stdout, "error_code=%s\nerror_message=%s\n", model.ErrUnrecoverable, sanitize(err.Error()))
		return exitUnrecoverable
	}
	if err := os.WriteFile(filepath.Join(work, "input", "rtp-input.bin"), fixtures.EncodeRTPStream(rtp.Packets), 0o644); err != nil {
		fmt.Fprintf(stdout, "error_code=%s\nerror_message=%s\n", model.ErrUnrecoverable, sanitize(err.Error()))
		return exitUnrecoverable
	}

	opts := pipeline.Options{
		InputDir:          filepath.Join(work, "input"),
		PackageDir:        filepath.Join(work, "package"),
		TargetKbps:        f.TargetKbps,
		BandwidthKbps:     f.BandwidthKbps,
		CheckpointSamples: f.CheckpointSamples,
		Seed:              f.Seed,
		LossBurst:         f.LossBurst,
		Duplicate:         f.Duplicate,
		Reorder:           f.Reorder,
		Corrupt:           f.Corrupt,
	}
	if f.RotationSet {
		rot := f.Rotation
		opts.Rotation = &rot
	}

	if f.Resume {
		return runWithResume(opts, stdout)
	}
	return runOnce(opts, false, stdout)
}

// runWithResume interrupts the first pass, then resumes to completion. It runs
// both passes through the checkpoint store, so an already-complete store simply
// finishes without a second interruption, while a fresh one stops after two new
// blocks and continues from there.
func runWithResume(opts pipeline.Options, stdout io.Writer) int {
	opts.Resume = true
	opts.InterruptAfter = 2
	first, err := pipeline.Run(opts)
	if err == nil {
		// Fewer than two blocks were pending; the run already finished.
		printReport(stdout, first, true)
		return exitOK
	}
	var ie *pipeline.InterruptError
	if !errors.As(err, &ie) {
		return fail(err, first, stdout)
	}
	fmt.Fprintf(stdout, "interrupted_after_blocks=%d\n", ie.After)
	fmt.Fprintf(stdout, "interrupt_hash=%s\n", first.FinalHash)

	opts.InterruptAfter = 0
	return runOnce(opts, true, stdout)
}

func runOnce(opts pipeline.Options, resumed bool, stdout io.Writer) int {
	rep, err := pipeline.Run(opts)
	if err != nil {
		return fail(err, rep, stdout)
	}
	printReport(stdout, rep, resumed)
	return exitOK
}
