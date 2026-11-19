// Command mediq validates broadcast frame sequences and re-wraps them into a
// random-access MP4 index.
//
// Exit codes:
//
//	0  completed and verification passed
//	1  unexpected local failure
//	2  invalid arguments
//	3  input damaged or unrepairable
//	4  checkpoint does not belong to this job
//	5  output verification failed
//	130 interrupted
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

	"mediq/internal/fixture"
	"mediq/internal/index"
	"mediq/internal/model"
	"mediq/internal/pipeline"
	"mediq/internal/timeline"
)

const (
	exitOK      = 0
	exitLocal   = 1
	exitArgs    = 2
	exitDamaged = 3
	exitCkpt    = 4
	exitVerify  = 5
	exitKilled  = 130
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitArgs
	}
	switch args[0] {
	case "fixture":
		return runFixture(args[1:], stdout, stderr)
	case "process":
		return runProcess(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "mediq: unknown command %q\n", args[0])
		usage(stderr)
		return exitArgs
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mediq - broadcast frame sequence validator and indexer

usage:
  mediq fixture --dir DIR [--frames N] [--corrupt N] [--truncate-tail N] ...
  mediq process --input FILE --output FILE [--checkpoint FILE] [--chunk-frames N] [--queue N]
  mediq verify  --input FILE [--source FILE]

exit codes: 0 ok, 2 usage, 3 damaged input, 4 checkpoint mismatch, 5 verify failed
`)
}

func runFixture(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "output directory (required)")
	frames := fs.Int("frames", 1000, "number of frames per sequence")
	fps := fs.Int("fps", 30, "frames per second")
	timebase := fs.String("timebase", "90000/1", "capture timebase num/den")
	frameBytes := fs.Int("frame-bytes", 512, "bytes per synthetic frame")
	corrupt := fs.Int("corrupt", 0, "1-based frame ordinal whose payload is corrupted")
	wrapAt := fs.Int("wrap-at", 0, "1-based ordinal after which the timestamp wraps")
	duplicate := fs.Int("duplicate", 0, "1-based ordinal to duplicate")
	truncateTail := fs.Int("truncate-tail", 0, "records to cut from the tail")
	outOfOrder := fs.Int("out-of-order", 0, "1-based ordinal emitted one position early")
	slow := fs.Bool("slow", false, "also emit slow.seq for the backpressure demo")
	seed := fs.Int64("seed", 1, "payload seed")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if *dir == "" {
		fmt.Fprintln(stderr, "mediq fixture: --dir is required")
		return exitArgs
	}
	tb, err := model.ParseTimebase(*timebase)
	if err != nil {
		fmt.Fprintf(stderr, "mediq fixture: %v\n", err)
		return exitArgs
	}
	if *frames <= 0 {
		fmt.Fprintln(stderr, "mediq fixture: --frames must be positive")
		return exitArgs
	}
	res, err := fixture.Generate(fixture.Config{
		Dir: *dir, Frames: *frames, FPS: *fps, Timebase: tb,
		FrameBytes: *frameBytes, Corrupt: *corrupt, WrapAt: *wrapAt,
		Duplicate: *duplicate, TruncateTail: *truncateTail,
		OutOfOrder: *outOfOrder, SlowConsumer: *slow, Seed: *seed,
	})
	if err != nil {
		fmt.Fprintf(stderr, "mediq fixture: %v\n", err)
		return exitLocal
	}
	fmt.Fprintf(stdout, "fixture_dir=%s\nframes=%d\n", *dir, res.Frames)
	if res.CorruptSeq > 0 {
		fmt.Fprintf(stdout, "corrupt_frame=%d\n", res.CorruptSeq)
	}
	if res.WrapSeq > 0 {
		fmt.Fprintf(stdout, "wrap_frame=%d\n", res.WrapSeq)
	}
	if res.DuplicateSeq > 0 {
		fmt.Fprintf(stdout, "duplicate_frame=%d\n", res.DuplicateSeq)
	}
	if res.TruncatedBytes > 0 {
		fmt.Fprintf(stdout, "truncated_tail_bytes=%d\n", res.TruncatedBytes)
	}
	for _, name := range []string{"normal", "damaged", "wrap", "duplicate", "truncated", "slow"} {
		if p, ok := res.Files[name]; ok {
			fmt.Fprintf(stdout, "file_%s=%s\n", name, p)
		}
	}
	return exitOK
}

func runProcess(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("process", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "input frame sequence (required)")
	output := fs.String("output", "", "output MP4 index (required)")
	ckpt := fs.String("checkpoint", "", "checkpoint path (default OUTPUT.ckpt)")
	chunk := fs.Int("chunk-frames", 256, "samples per committed chunk")
	queue := fs.Int("queue", 8, "bounded queue depth")
	restart := fs.Bool("restart", false, "discard any checkpoint and start over")
	slowMs := fs.Int("slow-ms", 0, "artificial per-sample delay in ms")
	drift := fs.Int64("drift-tolerance", 450, "backwards jitter tolerance in 90 kHz ticks")
	reorder := fs.Int("reorder-window", 32, "max frames buffered for reordering")
	verbose := fs.Bool("v", false, "log every diagnosed fault")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if *input == "" || *output == "" {
		fmt.Fprintln(stderr, "mediq process: --input and --output are required")
		return exitArgs
	}
	if *chunk <= 0 || *queue <= 0 {
		fmt.Fprintln(stderr, "mediq process: --chunk-frames and --queue must be positive")
		return exitArgs
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logw := io.Discard
	if *verbose {
		logw = stderr
	}
	runner := pipeline.New(pipeline.Options{
		Input:          *input,
		Output:         *output,
		CheckpointPath: *ckpt,
		ChunkFrames:    *chunk,
		QueueDepth:     *queue,
		Restart:        *restart,
		SlowConsumer:   time.Duration(*slowMs) * time.Millisecond,
		TimelineOpts: timeline.Options{
			ReorderWindow:  *reorder,
			DriftTolerance: *drift,
		},
		Log: logw,
	})
	stats, err := runner.Run(ctx)
	if stats != nil {
		printStats(stdout, stats)
	}
	return classify(stats, err, stderr)
}

func printStats(w io.Writer, s *pipeline.Stats) {
	fmt.Fprintf(w, "frames_read=%d\n", s.FramesRead)
	fmt.Fprintf(w, "frames_indexed=%d\n", s.FramesIndexed)
	fmt.Fprintf(w, "frames_bad=%d\n", s.FramesBad)
	fmt.Fprintf(w, "frames_quarantined=%d\n", s.FramesQuarantined)
	if s.HasCorrupt {
		fmt.Fprintf(w, "first_corrupt_frame=%d\n", s.FirstCorruptFrame)
		fmt.Fprintf(w, "first_corrupt_offset=%d\n", s.FirstCorruptOffset)
	}
	fmt.Fprintf(w, "recovered_chunks=%d\n", s.RecoveredChunks)
	fmt.Fprintf(w, "resumed=%t\n", s.Resumed)
	if s.Resumed {
		fmt.Fprintf(w, "resume_offset=%d\n", s.ResumeOffset)
	}
	fmt.Fprintf(w, "resume_ms=%d\n", s.ResumeMs)
	fmt.Fprintf(w, "peak_queue_depth=%d\n", s.PeakQueueDepth)
	if s.OutputSHA256 != "" {
		fmt.Fprintf(w, "output_sha256=%s\n", s.OutputSHA256)
	}
	fmt.Fprintf(w, "index_valid=%t\n", s.IndexValid)
	for code, n := range s.QuarantineReasons {
		if n > 0 {
			fmt.Fprintf(w, "quarantined_%s=%d\n", code, n)
		}
	}
}

func classify(stats *pipeline.Stats, err error, stderr io.Writer) int {
	if err == nil {
		if stats != nil && stats.FramesBad > 0 {
			// Damaged frames were isolated and the rest indexed; that is the
			// documented outcome for a repairable delivery.
			fmt.Fprintln(stderr, "mediq: damaged input isolated; valid frames indexed")
		}
		return exitOK
	}
	fmt.Fprintf(stderr, "mediq: %v\n", err)
	switch {
	case errors.Is(err, context.Canceled):
		return exitKilled
	case errors.Is(err, model.ErrCheckpointMismatch):
		return exitCkpt
	case errors.Is(err, model.ErrVerifyFailed):
		return exitVerify
	case errors.Is(err, model.ErrDamagedInput):
		return exitDamaged
	}
	var fault *model.Fault
	if errors.As(err, &fault) {
		return exitDamaged
	}
	return exitLocal
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "MP4 index to verify (required)")
	source := fs.String("source", "", "source frame sequence to cross-check against")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}
	if *input == "" {
		fmt.Fprintln(stderr, "mediq verify: --input is required")
		return exitArgs
	}
	rep, err := index.VerifyFile(*input, index.VerifyOptions{SourcePath: *source})
	if err != nil {
		fmt.Fprintf(stderr, "mediq verify: %v\n", err)
		return exitLocal
	}
	fmt.Fprintf(stdout, "samples=%d\n", rep.Samples)
	fmt.Fprintf(stdout, "chunks=%d\n", rep.Chunks)
	fmt.Fprintf(stdout, "first_media_90k=%d\n", rep.FirstMedia)
	fmt.Fprintf(stdout, "last_media_90k=%d\n", rep.LastMedia)
	fmt.Fprintf(stdout, "timestamps_monotonic=%t\n", rep.TimestampMono)
	fmt.Fprintf(stdout, "source_faults=%d\n", len(rep.SourceFaults))
	fmt.Fprintf(stdout, "unindexed_source_frames=%d\n", rep.UnindexedSource)
	fmt.Fprintf(stdout, "output_sha256=%s\n", rep.OutputSHA256)
	fmt.Fprintf(stdout, "index_valid=%t\n", rep.IndexValid)
	for _, p := range rep.Problems {
		fmt.Fprintf(stdout, "problem=%s\n", p)
	}
	if !rep.IndexValid {
		return exitVerify
	}
	return exitOK
}
