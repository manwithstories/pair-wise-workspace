package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rtprelay/internal/relay"
	"rtprelay/internal/report"
	"rtprelay/internal/runner"
	"rtprelay/internal/stats"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "selftest":
		return runSelfTest(args[1:], stdout, stderr)
	case "stats":
		return runStats(args[1:], stdout, stderr)
	case "signal":
		return runSignal(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

type commonFlags struct {
	duration  time.Duration
	pps       int
	seed      int64
	jitterUS  int64
	fault     string
	inject    string
	maxJitter float64
	out       string
	timeout   time.Duration
}

func addCommonFlags(fs *flag.FlagSet, cfg *commonFlags) {
	fs.DurationVar(&cfg.duration, "duration", 4*time.Second, "test duration")
	fs.IntVar(&cfg.pps, "pps", 200, "RTP packets per second")
	fs.Int64Var(&cfg.seed, "seed", 7, "deterministic seed")
	fs.Int64Var(&cfg.jitterUS, "jitter-us", 800, "synthetic pacing jitter in microseconds")
	fs.StringVar(&cfg.fault, "fault", "", "faults: drop=0.05,dup=0.01,reorder=0.01")
	fs.StringVar(&cfg.inject, "inject-candidate", "", "candidate to validate, e.g. 10.0.0.5:40000")
	fs.Float64Var(&cfg.maxJitter, "max-jitter-ms", 0, "fail when jitter p95 exceeds this value")
	fs.StringVar(&cfg.out, "out", "out/report.json", "JSON report path")
	fs.DurationVar(&cfg.timeout, "timeout", 15*time.Second, "signaling/connect timeout")
}

func (c commonFlags) options() (runner.Options, error) {
	if c.duration <= 0 || c.pps <= 0 || c.timeout <= 0 || c.jitterUS < 0 || c.maxJitter < 0 {
		return runner.Options{}, fmt.Errorf("duration, pps and timeout must be positive; jitter values cannot be negative")
	}
	faults, err := parseFaults(c.fault)
	if err != nil {
		return runner.Options{}, err
	}
	return runner.Options{
		Duration: c.duration, PPS: c.pps, Seed: c.seed,
		Jitter: time.Duration(c.jitterUS) * time.Microsecond,
		Faults: faults, InjectCandidate: c.inject, MaxJitterMS: c.maxJitter,
		Out: c.out, Timeout: c.timeout,
	}, nil
}

func runSelfTest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg commonFlags
	addCommonFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	opts, err := cfg.options()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := runner.Run(ctx, opts)
	result.Report.Print(stdout)
	if err := result.Report.Write(opts.Out); err != nil {
		fmt.Fprintf(stderr, "write report: %v\n", err)
		return 2
	}
	return result.ExitCode
}

func runSignal(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("signal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg commonFlags
	addCommonFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return 2
	}
	opts, err := cfg.options()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := runner.RunSignal(ctx, opts)
	result.Report.Print(stdout)
	if err := result.Report.Write(opts.Out); err != nil {
		fmt.Fprintf(stderr, "write report: %v\n", err)
		return 2
	}
	return result.ExitCode
}

func runStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("in", "out/packets.jsonl", "per-packet JSONL input")
	out := fs.String("out", "out/recheck.json", "recomputed report path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *input == "" || *out == "" {
		return 2
	}
	summary, records, err := stats.ReadJSONL(*input, 48000)
	if err != nil {
		fmt.Fprintf(stderr, "read packet log: %v\n", err)
		return 2
	}
	recheck := report.NewRecheck(*input, records, summary)
	if err := recheck.Write(*out); err != nil {
		fmt.Fprintf(stderr, "write recheck: %v\n", err)
		return 2
	}
	recheck.Print(stdout)
	return 0
}

func parseFaults(spec string) (relay.Faults, error) {
	var faults relay.Faults
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, valueText, ok := strings.Cut(part, "=")
		if !ok {
			return faults, fmt.Errorf("invalid fault %q; expected name=probability", part)
		}
		value, err := strconv.ParseFloat(valueText, 64)
		if err != nil || value < 0 || value > 1 {
			return faults, fmt.Errorf("invalid fault probability %q", valueText)
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "drop":
			faults.Drop = value
		case "dup", "duplicate":
			faults.Duplicate = value
		case "reorder":
			faults.Reorder = value
		default:
			return faults, fmt.Errorf("unknown fault %q", key)
		}
	}
	return faults, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: rtprelay <selftest|stats|signal> [flags]")
	fmt.Fprintln(w, "  selftest  run signaling, ICE, SRTP, RTP relay and report")
	fmt.Fprintln(w, "  stats     recompute jitter from out/packets.jsonl")
	fmt.Fprintln(w, "  signal    run an in-process signaling round trip")
}
