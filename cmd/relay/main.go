// Command relay is the acceptance entry point for the relay pipeline.
//
// It offers two subcommands:
//
//	selfcheck  runs the full pipeline and asserts the acceptance invariants.
//	demo        runs the pipeline twice into separate directories so the two
//	            SHA256SUMS manifests can be compared with cmp(1).
//
// Exit codes: 0 success, 2 unrecoverable input, 3 cancelled and resumable,
// 4 digest mismatch.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"relay.local/relay/internal/relay"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage()
		return relay.ExitBadInput
	}
	command := args[0]
	switch command {
	case "selfcheck", "demo":
	case "-h", "--help", "help":
		usage()
		return relay.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "relay: unknown command %q\n", command)
		usage()
		return relay.ExitBadInput
	}
	opts, err := parseFlags(command, args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return relay.ExitBadInput
	}
	return relay.Run(ctx, command, opts, os.Stdout)
}

func parseFlags(command string, args []string) (relay.Options, error) {
	var opts relay.Options
	var policy string
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&opts.Seed, "seed", 41, "deterministic fixture seed")
	fs.IntVar(&opts.Workers, "workers", 4, "worker pool concurrency")
	fs.IntVar(&opts.Inflight, "inflight", 12, "worker queue capacity")
	fs.StringVar(&opts.Out, "out", "", "output directory (required)")
	fs.IntVar(&opts.Width, "width", 0, "frame width (0 uses the fixture default)")
	fs.IntVar(&opts.Height, "height", 0, "frame height (0 uses the fixture default)")
	fs.IntVar(&opts.Segments, "segments", 0, "number of output segments (0 uses the default)")
	fs.StringVar(&policy, "policy", "", "metadata policy, e.g. rotation=90,crop=2,2,2,2")
	fs.IntVar(&opts.CancelAfter, "cancel-after", 0, "simulate cancellation after N frames")
	fs.BoolVar(&opts.Resume, "resume", false, "resume a previously cancelled run")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.Out == "" {
		return opts, fmt.Errorf("--out is required")
	}
	m, err := relay.ParsePolicy(policy)
	if err != nil {
		return opts, err
	}
	opts.Policy = m
	return opts, nil
}

func usage() {
	fmt.Fprint(os.Stderr, `relay - deterministic cross-node media relay

usage:
  relay selfcheck --seed 41 --workers 4 --inflight 12 --out DIR
  relay demo      --seed 41 --workers 4 --inflight 12 --out DIR

exit codes: 0 ok, 2 unrecoverable input, 3 cancelled, 4 digest mismatch
`)
}
