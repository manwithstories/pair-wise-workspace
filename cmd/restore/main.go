// Command restore drives the local archive-restoration pipeline. The
// only subcommand is "demo", which synthesizes its own carriers and video
// anchors in-process (no external files or services) and prints the
// quantified summary as deterministic key=value lines.
//
// Exit codes: 0 completion (including reported degradation), 2 no complete
// sample frame could be recovered, 64 invalid parameters.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"audiorestore/internal/model"
	"audiorestore/internal/pipeline"
)

const (
	exitOK        = 0
	exitNoFrames  = 2
	exitBadParams = 64
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printUsage(os.Stdout)
		return exitOK
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		printUsage(os.Stderr)
		return exitBadParams
	}
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: restore demo --case clean|damaged|recover --seed N")
	fmt.Fprintln(w, "  [--drop N] [--drop-per-group N] [--corrupt N]")
	fmt.Fprintln(w, "  [--truncate-bytes N] [--parity N]")
}

func runDemo(args []string) int {
	opts, err := parseDemoArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parameter error: %v\n", err)
		return exitBadParams
	}
	carriers, anchors, cfg, err := buildDemo(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parameter error: %v\n", err)
		return exitBadParams
	}
	run := pipeline.New(carriers, anchors, opts.refRate(), cfg)
	summary, err := run.Execute()
	if err != nil {
		var te model.TrackedError
		if errors.As(err, &te) && te.Kind == model.ErrNoFrames {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
			return exitNoFrames
		}
		fmt.Fprintf(os.Stderr, "pipeline error: %v\n", err)
		return exitBadParams
	}
	fmt.Print(summary.RenderText())
	return exitOK
}

type demoOptions struct {
	Case          string
	Seed          int64
	Drop          int
	DropPerGroup  int
	Corrupt       int
	TruncateBytes int
	Parity        int
}

func parseDemoArgs(args []string) (demoOptions, error) {
	o := demoOptions{Case: "clean", Seed: 1}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--case":
			v, err := next()
			if err != nil {
				return o, err
			}
			if v != "clean" && v != "damaged" && v != "recover" {
				return o, fmt.Errorf("--case must be clean|damaged|recover, got %q", v)
			}
			o.Case = v
		case "--seed":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return o, fmt.Errorf("--seed must be a non-negative integer, got %q", v)
			}
			o.Seed = n
		case "--drop":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return o, fmt.Errorf("--drop must be non-negative, got %q", v)
			}
			o.Drop = n
		case "--drop-per-group":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return o, fmt.Errorf("--drop-per-group must be non-negative, got %q", v)
			}
			o.DropPerGroup = n
		case "--corrupt":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return o, fmt.Errorf("--corrupt must be non-negative, got %q", v)
			}
			o.Corrupt = n
		case "--truncate-bytes":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return o, fmt.Errorf("--truncate-bytes must be non-negative, got %q", v)
			}
			o.TruncateBytes = n
		case "--parity":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return o, fmt.Errorf("--parity must be >= 1, got %q", v)
			}
			o.Parity = n
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
	}
	return o, nil
}

func (o demoOptions) refRate() model.SampleRate { return model.SampleRate(48000) }
