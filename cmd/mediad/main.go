// Command mediad is the terminal entry point. It has no GUI, browser, camera
// or microphone dependency: every feature is driven by flags and rendered to
// stdout, with exit codes 0 success, 1 usage, 2 data/processing failure.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"mediad/internal/fixtures"
	"mediad/internal/model"
	"mediad/internal/pipeline"
)

// exit codes are part of the CLI contract.
const (
	exitOK    = 0
	exitUsage = 1
	exitData  = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is testable: it never calls os.Exit.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "mediad 1.0.0")
		return exitOK
	case "help", "--help", "-h":
		printUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "mediad: unknown command %q\n", args[0])
		printUsage(stderr)
		return exitUsage
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `mediad - cross-node multi-track timeline verification

Usage:
  mediad demo [flags]

Flags:
  --seed int              deterministic fixture seed (default 42)
  --bandwidth-kbps float  requested delivery bitrate in kbps (default 1800)
  --loss-burst int        burst loss threshold in packets (default 4)
  --retry int             dependency-ordered retries per node (default 2)
  --out string            output directory (required)
  --corrupt-metadata      inject corrupted track metadata (exit 2)
  --scenario string       packet fixture: success|wrap|duplicate|reorder|burst-loss|interrupt
  --packets int           number of RTP packets to generate (default 120)
  --frames int            number of frames to compose and render (default 30)
  --no-frames             do not write PPM frame artifacts
  --render-scale int      raster downscale for verification renders (default 4)
  --resume-from string    resume an interrupted run from a prior manifest

Exit codes:
  0  success
  1  usage error
  2  data/processing error (metadata, transport, timeline, render, manifest)`)
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opt := pipeline.DefaultOptions()
	var (
		noFrames bool
	)
	fs.Int64Var(&opt.Seed, "seed", opt.Seed, "deterministic fixture seed")
	fs.Float64Var(&opt.BandwidthKbps, "bandwidth-kbps", opt.BandwidthKbps, "requested bitrate in kbps")
	fs.IntVar(&opt.LossBurst, "loss-burst", opt.LossBurst, "burst loss threshold in packets")
	fs.IntVar(&opt.Retry, "retry", opt.Retry, "retries per DAG node")
	fs.StringVar(&opt.Out, "out", "", "output directory")
	fs.BoolVar(&opt.CorruptMetadata, "corrupt-metadata", false, "inject corrupted metadata")
	fs.StringVar(&opt.Scenario, "scenario", opt.Scenario, "packet fixture scenario")
	fs.IntVar(&opt.Packets, "packets", opt.Packets, "RTP packets to generate")
	fs.IntVar(&opt.Frames, "frames", opt.Frames, "frames to compose and render")
	fs.BoolVar(&noFrames, "no-frames", false, "skip PPM frame artifacts")
	fs.IntVar(&opt.RenderScale, "render-scale", opt.RenderScale, "raster downscale")
	fs.StringVar(&opt.ResumeFrom, "resume-from", "", "resume from a prior manifest")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mediad: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if opt.Out == "" {
		fmt.Fprintln(stderr, "mediad: --out is required")
		return exitUsage
	}
	if opt.BandwidthKbps <= 0 {
		fmt.Fprintln(stderr, "mediad: --bandwidth-kbps must be positive")
		return exitUsage
	}
	if opt.Retry < 0 {
		fmt.Fprintln(stderr, "mediad: --retry must be >= 0")
		return exitUsage
	}
	if opt.RenderScale < 1 {
		fmt.Fprintln(stderr, "mediad: --render-scale must be >= 1")
		return exitUsage
	}
	if !validScenario(opt.Scenario) {
		fmt.Fprintf(stderr, "mediad: unknown scenario %q\n", opt.Scenario)
		return exitUsage
	}
	opt.WriteFrames = !noFrames

	res, err := pipeline.Run(opt)
	if res != nil {
		printResult(stdout, res)
	}
	if err != nil {
		code := model.ExitCode(err)
		fmt.Fprintln(stderr, err.Error())
		// Always make the contract explicit for data errors.
		if code == exitData {
			var me *model.Error
			if errors.As(err, &me) {
				fmt.Fprintf(stderr, "mediad: category=%s track=%s blocked_node=%s\n",
					me.Category, orNone(me.Track), orNone(firstNonEmpty(me.BlockingDep, res.BlockedNode)))
			}
		}
		return code
	}
	return exitOK
}

// printResult emits exactly the fields the acceptance command expects, one per
// line, followed by a short provenance section.
func printResult(w io.Writer, res *pipeline.Result) {
	m := res.Manifest.Metrics
	fmt.Fprintf(w, "target_kbps=%.2f\n", m.TargetKbps)
	fmt.Fprintf(w, "actual_kbps=%.2f\n", m.ActualKbps)
	fmt.Fprintf(w, "bitrate_error_pct=%.2f\n", m.BitrateErrorPct)
	fmt.Fprintf(w, "frame_count=%d\n", m.FrameCount)
	fmt.Fprintf(w, "psnr_mean_db=%.2f\n", m.PSNRMeanDB)
	fmt.Fprintf(w, "ssim_mean=%.4f\n", m.SSIMMean)
	fmt.Fprintf(w, "manifest_drift_ms=%d\n", m.ManifestDriftMs)
	fmt.Fprintf(w, "duplicate_packets=%d\n", m.DuplicatePackets)
	fmt.Fprintf(w, "reordered_packets=%d\n", m.ReorderedPackets)
	fmt.Fprintf(w, "lost_packets=%d\n", m.LostPackets)
	fmt.Fprintf(w, "blocked_node=%s\n", orNone(firstNonEmpty(m.BlockedNode, res.BlockedNode)))
	fmt.Fprintf(w, "wrapped=%t\n", res.Transport.Wrapped)
	fmt.Fprintf(w, "out=%s\n", res.OutDir)
}

func validScenario(s string) bool {
	for _, known := range fixtures.AllScenarios() {
		if known == s {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" && v != "none" {
			return v
		}
	}
	return ""
}
