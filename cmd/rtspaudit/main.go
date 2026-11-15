package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type commonOptions struct {
	fixture        string
	frames         int
	loss           float64
	reorder        float64
	duplicate      float64
	seed           int64
	out            string
	silenceTimeout time.Duration
	maxReconnects  int
	policy         string
	sessionTimeout time.Duration
	stallAfter     int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "pull":
		fs := flag.NewFlagSet("pull", flag.ContinueOnError)
		fs.SetOutput(stderr)
		opts := addCommonFlags(fs)
		transport := fs.String("transport", "tcp", "tcp or udp")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		if err := validateCommon(opts); err != nil {
			fmt.Fprintln(stderr, "argument error:", err)
			return exitUsage
		}
		if *transport != "tcp" && *transport != "udp" {
			fmt.Fprintf(stderr, "argument error: unsupported transport %q\n", *transport)
			return exitUsage
		}
		return runPull(ctx, *opts, *transport, stdout, stderr)
	case "compare":
		fs := flag.NewFlagSet("compare", flag.ContinueOnError)
		fs.SetOutput(stderr)
		opts := addCommonFlags(fs)
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		if err := validateCommon(opts); err != nil {
			fmt.Fprintln(stderr, "argument error:", err)
			return exitUsage
		}
		return runCompare(ctx, *opts, stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return exitUsage
	}
}

func addCommonFlags(fs *flag.FlagSet) *commonOptions {
	opts := &commonOptions{}
	fs.StringVar(&opts.fixture, "fixture", "auto", "fixture source (only auto)")
	fs.IntVar(&opts.frames, "frames", 120, "generated frame count")
	fs.Float64Var(&opts.loss, "loss", 0.02, "packet loss probability")
	fs.Float64Var(&opts.reorder, "reorder", 0.01, "adjacent reorder probability")
	fs.Float64Var(&opts.duplicate, "dup", 0.01, "duplicate packet probability")
	fs.Int64Var(&opts.seed, "seed", 7, "deterministic impairment seed")
	fs.StringVar(&opts.out, "out", "", "JSON output path")
	fs.DurationVar(&opts.silenceTimeout, "silence-timeout", time.Second, "UDP silence threshold")
	fs.IntVar(&opts.maxReconnects, "max-reconnects", 2, "UDP silence reconnect limit")
	fs.StringVar(&opts.policy, "server-policy", "strict", "mock policy: strict or lenient")
	fs.DurationVar(&opts.sessionTimeout, "session-timeout", 0, "mock RTSP session timeout")
	fs.IntVar(&opts.stallAfter, "stall-after", 0, "mock packets before a deliberate stall")
	return opts
}

func validateCommon(opts *commonOptions) error {
	if opts.fixture != "auto" {
		return fmt.Errorf("unsupported fixture %q", opts.fixture)
	}
	if opts.frames <= 0 || opts.frames > 10000 {
		return fmt.Errorf("frames must be in 1..10000")
	}
	for name, value := range map[string]float64{
		"loss": opts.loss, "reorder": opts.reorder, "dup": opts.duplicate,
	} {
		if value < 0 || value > 1 {
			return fmt.Errorf("%s must be in 0..1", name)
		}
	}
	if opts.silenceTimeout <= 0 {
		return fmt.Errorf("silence-timeout must be positive")
	}
	if opts.maxReconnects < 0 {
		return fmt.Errorf("max-reconnects must be non-negative")
	}
	if opts.policy != "strict" && opts.policy != "lenient" {
		return fmt.Errorf("server-policy must be strict or lenient")
	}
	if opts.sessionTimeout < 0 || opts.stallAfter < 0 || opts.stallAfter > opts.frames {
		return fmt.Errorf("invalid timeout or stall-after")
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  rtspaudit pull --transport tcp|udp [flags]")
	fmt.Fprintln(w, "  rtspaudit compare [flags]")
	fmt.Fprintln(w, "fixtures and impairments are generated locally; only 127.0.0.1 is used")
}
