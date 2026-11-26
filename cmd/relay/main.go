// Command relay drives the bounded fMP4 fragment pipeline as a terminal
// demo. It self-generates deterministic fixtures, runs ingest -> timeline
// repair -> bounded stream -> keyframe index -> independent verification,
// and can interrupt and resume a run to prove checkpoint equivalence.
package main

import (
	"fmt"
	"os"
	"time"
)

// Exit codes mandated by the acceptance contract.
const (
	exitOK       = 0
	exitArgs     = 2
	exitVerify   = 3
	exitWrite    = 4
)

type options struct {
	selfGenerate   bool
	profile        string
	queueBytes     int64
	consumerDelay  time.Duration
	interruptAfter int
	resume         bool
	outDir         string
	refDir         string
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitArgs
	}
	if args[0] != "demo" {
		fmt.Fprintf(os.Stderr, "unknown command %q (expected: demo)\n", args[0])
		return exitArgs
	}
	opts, err := parseFlags(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		usage()
		return exitArgs
	}
	if err := execute(opts); err != nil {
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
		if e, ok := err.(*codedError); ok {
			return e.code
		}
		return exitWrite
	}
	return exitOK
}

type codedError struct {
	code int
	msg  string
}

func (e *codedError) Error() string { return e.msg }

func usage() {
	fmt.Fprint(os.Stderr, `usage: relay demo --self-generate --profile all
  --queue-bytes <bytes> --consumer-delay <duration>
  --interrupt-after <n> --resume [--out-dir <path>] [--ref-dir <path>]
`)
}

func parseFlags(args []string) (options, error) {
	opts := options{
		profile:    "all",
		queueBytes: 1 << 20,
		outDir:     "relay-out",
		refDir:     "relay-reference",
	}
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
		case "--self-generate":
			opts.selfGenerate = true
		case "--resume":
			opts.resume = true
		case "--profile":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.profile = v
		case "--queue-bytes":
			v, err := next()
			if err != nil {
				return opts, err
			}
			n, err := parseBytes(v)
			if err != nil {
				return opts, err
			}
			opts.queueBytes = n
		case "--consumer-delay":
			v, err := next()
			if err != nil {
				return opts, err
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				return opts, fmt.Errorf("invalid consumer-delay %q: %w", v, err)
			}
			opts.consumerDelay = d
		case "--interrupt-after":
			v, err := next()
			if err != nil {
				return opts, err
			}
			n, err := parseInt(v)
			if err != nil {
				return opts, err
			}
			opts.interruptAfter = n
		case "--out-dir":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.outDir = v
		case "--ref-dir":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.refDir = v
		default:
			return opts, fmt.Errorf("unknown flag %q", a)
		}
	}
	if !opts.selfGenerate {
		return opts, fmt.Errorf("--self-generate is required for the demo")
	}
	if opts.queueBytes <= 0 {
		return opts, fmt.Errorf("--queue-bytes must be positive")
	}
	return opts, nil
}
