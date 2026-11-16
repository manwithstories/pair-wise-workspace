// Command relay is a loopback-only WebRTC RTP data-path harness.
//
// It generates a deterministic RTP fixture, pushes it through a bounded
// forwarder into a sender PeerConnection, receives it on a second
// PeerConnection over 127.0.0.1/::1, and reports jitter, loss, duplicate and
// reordering statistics as human-readable text and stable JSON.
//
// Usage:
//
//	relay demo  --packets 2000 --interval-ms 1 --reorder 32 \
//	            --duplicate-rate 0.02 --loss-rate 0.01 --json
//	relay serve --addr 127.0.0.1:9000
//	relay probe --addr 127.0.0.1:9000 --json
//
// Exit codes: 0 success, 2 invalid arguments, 3 signaling/ICE/DTLS failure,
// 4 metric thresholds not met, 130 interrupted.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/manwithstories/pair-wise-workspace/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is separated from main so tests can exercise argument handling without
// spawning a process.
func run(args []string) int {
	opts, err := cli.Parse(args, os.Stderr)
	if err != nil {
		if errors.Is(err, cli.ErrUsage) {
			fmt.Fprintf(os.Stderr, "relay: %v\n", err)
			return cli.ExitUsage
		}
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return cli.ExitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := cli.Run(ctx, opts, os.Stdout, os.Stderr)
	if ctx.Err() != nil && code == cli.ExitOK {
		return cli.ExitInterrupted
	}
	return code
}
