// Command fieldcut is the terminal-only demo adapter for the field
// post-production pipeline. It synthesizes all inputs locally, runs the
// bounded deterministic pipeline and prints acceptance metrics.
//
// Exit codes: 0 success with verifiable artifacts; 2 invalid arguments or
// parameters; 1 processing or invariant failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/clearliu/fieldcut/internal/demo"
	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/fixtures"
)

const usage = `fieldcut - offline field-recording segment pipeline

Usage:
  fieldcut demo [flags]

Flags:
  --seed int            deterministic generator seed (default 41)
  --duration duration   generated capture duration (default 300s)
  --queue-bytes int     hard per-queue byte budget (default 8388608)
  --queue-items int     hard in-flight item budget (default 4096)
  --late-window duration replay look-back window (default 4s)
  --inject-loss float   RTP packet loss fraction 0..1 (default 0.02)
  --slow-consumer-ms int artificial sink delay per item (default 25)
  --case string         fault case: normal|loss|duplicate|late-out-of-order|
                        timestamp-wrap|truncated-box|late-outside-window

Exit codes: 0 ok, 2 bad arguments, 1 processing/invariant failure.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] != "demo" {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic generator seed")
	duration := fs.Duration("duration", 300*time.Second, "capture duration")
	queueBytes := fs.Int("queue-bytes", 8<<20, "hard per-queue byte budget")
	queueItems := fs.Int("queue-items", 4096, "hard in-flight item budget")
	lateWindow := fs.Duration("late-window", 4*time.Second, "replay look-back window")
	injectLoss := fs.Float64("inject-loss", 0.02, "RTP loss fraction")
	slowConsumer := fs.Int("slow-consumer-ms", 25, "sink delay per item in ms")
	caseName := fs.String("case", string(fixtures.CaseNormal), "fault case to inject")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}

	opts := demo.Options{
		Seed:           *seed,
		Duration:       *duration,
		QueueBytes:     *queueBytes,
		QueueItems:     *queueItems,
		LateWindow:     *lateWindow,
		InjectLoss:     *injectLoss,
		SlowConsumerMs: *slowConsumer,
		Case:           fixtures.Case(*caseName),
	}
	if !validCase(opts.Case) {
		fmt.Fprintf(os.Stderr, "unknown --case %q\n", *caseName)
		os.Exit(2)
	}
	if clock.FromDuration(opts.Duration) > clock.Ticks(clock.MaxLateWindowTicks)*10 {
		fmt.Fprintf(os.Stderr, "duration exceeds hard input cap\n")
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := demo.Run(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipeline failed: %v\n", err)
		os.Exit(exitCodeFor(err))
	}
	printMetrics(opts, m)
	os.Exit(0)
}

func validCase(c fixtures.Case) bool {
	switch c {
	case fixtures.CaseNormal, fixtures.CaseLoss, fixtures.CaseDuplicate,
		fixtures.CaseLateOutOfOrder, fixtures.CaseTimestampWrap,
		fixtures.CaseTruncatedBox, fixtures.CaseLateOutsideWindow:
		return true
	}
	return false
}

// exitCodeFor maps validation errors to 2 and everything else to 1.
func exitCodeFor(err error) int {
	switch {
	case errorIs(err, clock.ErrInvalidArgument), errorIs(err, clock.ErrPathTraversal),
		errorIs(err, clock.ErrDuplicateSeq), errorIs(err, clock.ErrManifestTooLarge):
		return 2
	default:
		return 1
	}
}

func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func printMetrics(opts demo.Options, m demo.Metrics) {
	fmt.Printf("fieldcut demo case=%s seed=%d\n", opts.Case, opts.Seed)
	fmt.Printf("av_sync_ms_p95=%d\n", m.AVSyncP95Ms)
	fmt.Printf("packet_recovery_rate=%.3f\n", m.PacketRecoveryRate)
	fmt.Printf("pipeline_latency_p95_ms=%d\n", m.PipelineLatencyP95Ms)
	fmt.Printf("peak_buffer_bytes=%d\n", m.PeakBufferBytes)
	fmt.Printf("late_replay_count=%d\n", m.LateReplayCount)
	fmt.Printf("index_seek_misses=%d\n", m.IndexSeekMisses)
	fmt.Printf("index_sha256=%s\n", m.IndexSHA256)
	fmt.Printf("quarantined=%d untrusted_events=%d processed_packets=%d gap_packets=%d\n",
		m.Quarantined, m.UntrustedEvents, m.ProcessedPackets, m.GapPackets)
}
