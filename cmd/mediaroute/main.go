// Command mediaroute is the terminal entry point for the regional live-media
// routing demo. It generates self-contained fixtures, scans them into a
// timeline index, streams the index through audio normalisation and RTP
// recovery, and prints a verifiable JSON report.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"mediaroute/internal/demo"
	merr "mediaroute/internal/errors"
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
	case "scan":
		return runScan(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "version":
		fmt.Println("mediaroute 1.0.0")
		return 0
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mediaroute: unknown command %q\n", args[0])
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediaroute — regional live-media routing demo

Usage:
  mediaroute demo   [flags]   generate fixtures, scan, stream and report
  mediaroute scan   [flags]   scan a manifest into an index and report
  mediaroute verify [flags]   re-scan and compare artifact digests
  mediaroute version

Run "mediaroute <command> -h" for command flags.
`)
}

// commonFlags are shared by every subcommand.
type commonFlags struct {
	workDir string
	jsonOut bool
	timeout time.Duration
	quiet   bool
}

func newFlagSet(name string) (*flag.FlagSet, *commonFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cf := &commonFlags{}
	fs.StringVar(&cf.workDir, "dir", "", "working directory for generated fixtures and cache (default: temp dir)")
	fs.BoolVar(&cf.jsonOut, "json", false, "emit the report as JSON")
	fs.DurationVar(&cf.timeout, "timeout", 2*time.Minute, "overall run timeout")
	fs.BoolVar(&cf.quiet, "quiet", false, "suppress human-readable progress on stderr")
	return fs, cf
}

func runDemo(args []string) int {
	fs, cf := newFlagSet("demo")
	seed := fs.Int64("seed", 42, "fixture generation seed")
	repeat := fs.Int("repeat", 1, "number of runs over the same fixtures")
	lossBurst := fs.Int("loss-burst", 0, "drop this many consecutive RTP packets once per track")
	truncateTail := fs.Int("truncate-tail", 0, "cut this many bytes from the video tail")
	reorderWindow := fs.Int("reorder-window", 64, "RTP reorder window in packets")
	keep := fs.Bool("keep", false, "keep the working directory after the run")
	reuse := fs.Bool("reuse", false, "keep media already present in --dir instead of regenerating it")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	opts := demo.Options{
		WorkDir:       cf.workDir,
		Seed:          *seed,
		Repeat:        *repeat,
		LossBurst:     *lossBurst,
		TruncateTail:  *truncateTail,
		ReorderWindow: *reorderWindow,
		KeepWorkDir:   *keep,
		ReuseExisting: *reuse,
		Quiet:         cf.quiet,
		JSON:          cf.jsonOut,
		Progress:      os.Stderr,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if cf.timeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, cf.timeout)
		defer stop()
	}
	res, err := demo.Run(ctx, opts)
	if res != nil {
		demo.Print(os.Stdout, res, opts.JSON)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediaroute:", err)
		return merr.ExitCode(err)
	}
	if res != nil && res.ExitCode != 0 {
		return res.ExitCode
	}
	return 0
}

func runScan(args []string) int {
	fs, cf := newFlagSet("scan")
	manPath := fs.String("manifest", "", "manifest file to scan")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *manPath == "" {
		fmt.Fprintln(os.Stderr, "mediaroute scan: --manifest is required")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dir := cf.workDir
	if dir == "" {
		dir = filepath.Dir(*manPath)
	}
	res, err := demo.ScanOnly(ctx, demo.ScanOptions{
		Manifest: *manPath,
		BaseDir:  dir,
		JSON:     cf.jsonOut,
		Progress: os.Stderr,
		Quiet:    cf.quiet,
	})
	if res != nil {
		demo.PrintMode(os.Stdout, res, cf.jsonOut, false)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediaroute:", err)
		return merr.ExitCode(err)
	}
	if res != nil && res.ExitCode != 0 {
		return res.ExitCode
	}
	return 0
}

func runVerify(args []string) int {
	fs, cf := newFlagSet("verify")
	manPath := fs.String("manifest", "", "manifest to re-scan and compare")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *manPath == "" {
		fmt.Fprintln(os.Stderr, "mediaroute verify: --manifest is required")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ok, detail, err := demo.Verify(ctx, *manPath, cf.workDir)
	if !cf.quiet {
		fmt.Fprintln(os.Stderr, detail)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediaroute:", err)
		return merr.ExitCode(err)
	}
	if !ok {
		return 2
	}
	return 0
}
