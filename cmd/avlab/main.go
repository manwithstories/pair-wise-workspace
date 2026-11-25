// Command avlab is the terminal entry point for the media laboratory. It only
// parses flags, invokes the library and prints key=value metrics; all logic
// lives in the internal packages.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"avlab/internal/lab"
	"avlab/internal/media"
)

// Exit codes follow the documented contract.
const (
	exitOK       = 0
	exitGeneral  = 1
	exitInvalid  = 2
	exitMetrics  = 3
	exitCanceled = 130
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitInvalid
	}
	switch args[0] {
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "avlab: unknown command %q\n", args[0])
		printUsage()
		return exitInvalid
	}
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Uint64("seed", 17, "deterministic PRNG seed")
	workers := fs.Int("workers", 4, "bounded worker pool size")
	queue := fs.Int("queue", 64, "bounded task queue depth")
	packets := fs.Uint64("packets", 4096, "RTP packets in the trace")
	repeat := fs.Int("repeat", 3, "repeat runs for hash equality")
	driftLimit := fs.Float64("drift-limit-ms", 20, "A/V drift p95 acceptance limit")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := lab.Config{
		Seed: *seed, Workers: *workers, Queue: *queue,
		Packets: *packets, Repeat: *repeat,
		Thresholds: lab.Thresholds{DriftP95MS: *driftLimit},
	}
	m, err := lab.Run(ctx, cfg)
	if err != nil {
		if media.IsCode(err, media.CodeCanceled) || ctx.Err() != nil {
			fmt.Println("status=canceled")
			return exitCanceled
		}
		fmt.Fprintf(os.Stderr, "avlab: invalid input: %v\n", err)
		return exitInvalid
	}

	printMetrics(m)
	if len(m.Failures) > 0 {
		for _, f := range m.Failures {
			fmt.Fprintf(os.Stderr, "avlab: threshold failure: %s\n", f)
		}
		return exitMetrics
	}
	fmt.Println("status=pass")
	return exitOK
}

func printMetrics(m *lab.Metrics) {
	fmt.Printf("roundtrip_equal=%s\n", m.RoundtripEqual)
	fmt.Printf("keyframe_hit=%s\n", m.KeyframeHit)
	fmt.Printf("segment_sha256_match=%s\n", m.SegmentSHA256Match)
	fmt.Printf("damaged_rejected=%s\n", m.DamagedRejected)
	fmt.Printf("burst_recovered=%s\n", m.BurstRecovered)
	fmt.Printf("deadline_skip=%d\n", m.DeadlineSkip)
	fmt.Printf("av_drift_p95_ms=%.3f\n", m.AVDriftP95MS)
	fmt.Printf("max_inflight=%d\n", m.MaxInflight)
	fmt.Printf("queue_peak=%d\n", m.QueuePeak)
	fmt.Printf("repeat_hash_equal=%t\n", m.RepeatHashEqual)
	fmt.Printf("cancel_workers=%d\n", m.CancelWorkers)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `avlab - low-latency media laboratory

usage:
  avlab verify [flags]

flags:
  --seed uint       deterministic PRNG seed (default 17)
  --workers int     bounded worker pool size (default 4, hard cap 32)
  --queue int       bounded task queue depth (default 64, hard cap 4096)
  --packets uint    RTP packets in the simulated trace (default 4096)
  --repeat int      repeat runs compared by hash (default 3)

exit codes:
  0   all metrics pass
  2   invalid arguments or input
  3   metrics below thresholds
  130 active cancellation (SIGINT/SIGTERM)`)
}
