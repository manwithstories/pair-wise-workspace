// rcbench is a terminal-only transcoding job tool: it generates synthetic
// YUV420p sources, encodes them with its own intra codec under a rate
// controller, measures PSNR, checkpoints progress and resumes jobs.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"rcbench/internal/report"
	"rcbench/internal/source"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: rcbench <gen|encode|resume|report|inspect> [flags]")
		return report.ExitUsage
	}
	var code int
	var err error
	switch args[0] {
	case "gen":
		code, err = cmdGen(args[1:], stdout)
	case "encode":
		code, err = cmdEncode(args[1:], stdout, stderr)
	case "resume":
		code, err = cmdResume(args[1:], stdout, stderr)
	case "report":
		code, err = cmdReport(args[1:], stdout)
	case "inspect":
		code, err = cmdInspect(args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		return report.ExitUsage
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
	}
	return code
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func cmdGen(args []string, stdout io.Writer) (int, error) {
	fs := newFlagSet("gen", os.Stderr)
	seed := fs.Int64("seed", 1, "random seed")
	frames := fs.Int("frames", 240, "number of frames")
	width := fs.Int("width", 640, "frame width")
	height := fs.Int("height", 360, "frame height")
	out := fs.String("out", "", "output .yuv path")
	if err := fs.Parse(args); err != nil {
		return report.ExitUsage, err
	}
	if *out == "" {
		return report.ExitUsage, fmt.Errorf("--out is required")
	}
	path, err := safePath(*out)
	if err != nil {
		return report.ExitUsage, err
	}
	cfg := source.Config{Seed: *seed, Frames: *frames, Width: *width, Height: *height}
	if err := cfg.Validate(); err != nil {
		return report.ExitUsage, err
	}
	sum, err := source.GenerateFile(path, cfg)
	if err != nil {
		return report.ExitUsage, err
	}
	cut, na, nb := source.Layout(cfg.Frames)
	fmt.Fprintf(stdout, "wrote %s: %d frames %dx%d, sha256=%s\n", path, cfg.Frames, cfg.Width, cfg.Height, sum)
	fmt.Fprintf(stdout, "layout: hard_cut@%d noise_a=[%d,%d) noise_b=[%d,%d)\n", cut, na[0], na[1], nb[0], nb[1])
	return report.ExitOK, nil
}

func cmdReport(args []string, stdout io.Writer) (int, error) {
	fs := newFlagSet("report", os.Stderr)
	jobDir := fs.String("job", "", "job directory")
	asJSON := fs.Bool("json", false, "emit JSON")
	floor := fs.Float64("psnr-floor", 0, "minimum per-segment PSNR dB")
	spread := fs.Float64("psnr-spread", 0, "maximum PSNR spread dB")
	maxDev := fs.Float64("max-deviation-pct", 8, "maximum bitrate deviation percent")
	if err := fs.Parse(args); err != nil {
		return report.ExitUsage, err
	}
	if *jobDir == "" {
		return report.ExitUsage, fmt.Errorf("--job is required")
	}
	path, err := safePath(*jobDir)
	if err != nil {
		return report.ExitUsage, err
	}
	rep, err := report.Build(path)
	if err != nil {
		return report.ExitUsage, err
	}
	if *asJSON {
		if err := rep.WriteJSON(stdout); err != nil {
			return report.ExitUsage, err
		}
	} else {
		rep.WriteText(stdout)
	}
	code, reasons := rep.Evaluate(*floor, *spread, *maxDev)
	for _, r := range reasons {
		fmt.Fprintf(stdout, "threshold: %s\n", r)
	}
	return code, nil
}

func cmdInspect(args []string, stdout io.Writer) (int, error) {
	fs := newFlagSet("inspect", os.Stderr)
	jobDir := fs.String("job", "", "job directory")
	if err := fs.Parse(args); err != nil {
		return report.ExitUsage, err
	}
	if *jobDir == "" {
		return report.ExitUsage, fmt.Errorf("--job is required")
	}
	path, err := safePath(*jobDir)
	if err != nil {
		return report.ExitUsage, err
	}
	return inspectJob(path, stdout)
}
