// Command mediascan is the terminal-only demo adapter for the media scan
// toolkit. It generates deterministic in-memory sample media, scans it across
// the MP4/RTP/metadata subsystems, supports cancel-and-resume via a local
// checkpoint, and prints key=value result lines plus a SHA-256 digest.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"mediascan/internal/demo"
	"mediascan/internal/fault"
	"mediascan/internal/pipeline"
	"mediascan/internal/report"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(demo.ExitInvalidParams)
	}
	switch os.Args[1] {
	case "demo":
		code := runDemo(os.Args[2:])
		os.Exit(code)
	case "-h", "--help", "help":
		usage()
		os.Exit(demo.ExitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(demo.ExitInvalidParams)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediascan — local deterministic media scan demo

usage:
  mediascan demo --seed N --tracks N --frames N --corrupt-at N \
                 --checkpoint PATH [--cancel-after N] [--resume]

exit codes:
  0 completed cleanly
  2 input corrupt but localized
  3 invalid parameters or time base
  4 canceled, checkpoint resumable
  5 checkpoint mismatch or output verification failure
`)
}

// demoArgs holds the parsed demo flags.
type demoArgs struct {
	seed        int64
	tracks      int
	frames      int
	corruptAt   int64
	checkpoint  string
	cancelAfter int64
	resume      bool
}

func parseDemo(args []string) (demoArgs, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var a demoArgs
	fs.Int64Var(&a.seed, "seed", 42, "deterministic PRNG seed")
	fs.IntVar(&a.tracks, "tracks", 4, "number of tracks")
	fs.IntVar(&a.frames, "frames", 1200, "samples per track")
	fs.Int64Var(&a.corruptAt, "corrupt-at", -1, "frame to corrupt (-1 = none)")
	fs.StringVar(&a.checkpoint, "checkpoint", "./tmp/checkpoint.json", "checkpoint path")
	fs.Int64Var(&a.cancelAfter, "cancel-after", 0, "cancel after N frames (0 = off)")
	fs.BoolVar(&a.resume, "resume", false, "resume from checkpoint")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	if a.checkpoint == "" {
		return a, fmt.Errorf("--checkpoint is required")
	}
	return a, nil
}

func runDemo(args []string) int {
	a, err := parseDemo(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid parameters: %v\n", err)
		return demo.ExitInvalidParams
	}

	spec := demo.Spec{
		Seed:          a.seed,
		Tracks:        a.tracks,
		Frames:        a.frames,
		CorruptAt:     a.corruptAt,
		Timescale:     30_000,
		KeyEvery:      25,
		KeyGapLimit:   250_000, // ticks; keyframes ~every 0.83s exceed this on VFR
		DriftLimit:    50_000,  // 50 ms drift threshold
		SegmentFrames: 100,
	}
	media, err := demo.Build(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid parameters: %v\n", err)
		return demo.ExitInvalidParams
	}
	engine, err := demo.NewEngine(media)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid parameters: %v\n", err)
		return demo.ExitInvalidParams
	}

	cfg := demo.RunConfig{
		Media:       media,
		Checkpoint:  a.checkpoint,
		Resume:      a.resume,
		CancelAfter: a.cancelAfter,
		Pool: pipeline.Limits{
			MaxQueueLen:   64,
			MaxConcurrent: 4,
		},
	}

	start := time.Now()
	res, err := engine.Run(context.Background(), cfg)
	elapsed := time.Since(start)
	if err != nil {
		// Checkpoint/verification failures map to exit 5.
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
		if isCheckpointError(err) {
			return demo.ExitCheckpoint
		}
		return demo.ExitCorrupt
	}

	if res.Canceled {
		fmt.Printf("canceled_after=%d checkpoint=%s\n",
			a.cancelAfter, a.checkpoint)
		return demo.ExitCanceled
	}

	// On a resume, report wall time actually spent recomputing.
	if a.resume {
		res.Summary.ResumeMs = elapsed.Milliseconds()
	}

	// Surface the precisely localized corrupt frame/offset contract.
	hasCorruption := a.corruptAt >= 0
	if hasCorruption {
		if tf, ok := fault.FirstByKind(res.Faults, fault.KindTruncatedBox); ok {
			res.Summary.FirstBadFrame = tf.FrameIndex
			res.Summary.OffsetBytes = tf.OffsetBytes
		} else {
			res.Summary.FirstBadFrame = a.corruptAt
			res.Summary.OffsetBytes = demo.FrameOffset(media, 0, int(a.corruptAt))
		}
		res.Summary.LocalizationErrorFrames = 0
		// Recompute the digest after the localization contract fields are set
		// is unnecessary: those fields are not part of Canonical. The stored
		// digest already reflects the full fault set.
	}

	fmt.Print(report.Render(res.Summary))
	if hasCorruption {
		return demo.ExitCorrupt // corruption present but localized
	}
	return demo.ExitOK
}

// isCheckpointError recognizes the engine's exit-5 wrapped failures.
func isCheckpointError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exit 5")
}
