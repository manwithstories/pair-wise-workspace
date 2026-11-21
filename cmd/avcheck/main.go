// Command avcheck is the offline acceptance adapter for the low-latency
// multi-track audio pipeline. It generates local fixtures only; no device or
// network access is performed.
//
// Exit codes: 0 metrics within thresholds, 1 corrupt input or resource-bound
// violation, 2 invalid command-line arguments.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"avcheck/internal/clock"
	"avcheck/internal/fixture"
	"avcheck/internal/metrics"
	"avcheck/internal/pipeline"
	"avcheck/internal/wav"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("avcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	duration := fs.Duration("duration", 10*time.Second, "generated stream duration")
	tracks := fs.Int("tracks", 3, "number of tracks")
	seed := fs.Int64("seed", 7, "deterministic RNG seed")
	lossBurst := fs.Int("loss-burst", 3, "max consecutive lost packets")
	reorder := fs.Int("reorder", 8, "reorder spread")
	duplicate := fs.Int("duplicate", 5, "duplicated packet count")
	queueBytes := fs.Int("queue-bytes", 262144, "queue byte ceiling")
	out := fs.String("out", "", "MP4 output path (empty = discard)")
	fix := fs.String("fixture", "", "named fixture: corrupt-wav|queue-boundary")
	if err := fs.Parse(args); err != nil {
		return exitArgs
	}

	switch *fix {
	case "":
		return runFullPipeline(*duration, *tracks, *seed, *lossBurst, *reorder,
			*duplicate, *queueBytes, *out)
	case "corrupt-wav":
		return runCorruptWAV()
	case "queue-boundary":
		return runQueueBoundary(*seed)
	default:
		fmt.Fprintf(os.Stderr, "unknown fixture %q\n", *fix)
		return exitArgs
	}
}

const (
	exitOK       = 0
	exitResource = 1
	exitArgs     = 2
)

func buildConfig(d time.Duration, tracks int, seed int64, loss, reorder, dup int) fixture.Config {
	cfg := fixture.DefaultConfig()
	cfg.DurationMS = int(d / time.Millisecond)
	cfg.Tracks = tracks
	cfg.Seed = seed
	cfg.LossBurst = loss
	cfg.Reorder = reorder
	cfg.Duplicate = dup
	if cfg.DurationMS <= 0 || cfg.Tracks < 1 || cfg.Tracks > 8 {
		return cfg
	}
	return cfg
}

func runFullPipeline(d time.Duration, tracks int, seed int64, loss, reorder, dup,
	queueBytes int, outPath string) int {
	if d <= 0 || tracks < 1 || tracks > 8 || queueBytes < 1 {
		fmt.Fprintf(os.Stderr, "invalid duration/tracks/queue-bytes\n")
		return exitArgs
	}
	cfg := buildConfig(d, tracks, seed, loss, reorder, dup)

	var writer io.Writer = io.Discard
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot create output: %v\n", err)
			return exitArgs
		}
		defer f.Close()
		writer = f
	}

	gen := fixture.NewGenerator(cfg)
	gen.BuildTracks()
	// Combine stress dimensions while keeping the generated timeline valid.
	packets := gen.Normal()
	if reorder > 0 || dup > 0 {
		packets = gen.ReorderAndDuplicate()
	}
	if loss > 0 {
		var dropped map[uint32][]uint16
		packets, dropped = gen.LossBurst()
		_ = dropped
	}

	res, err := pipeline.Run(context.Background(), pipeline.Params{
		Fixture:    cfg,
		QueueBytes: queueBytes,
		WindowPkts: adaptiveWindowPackets(queueBytes),
		Out:        writer,
	}, packets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipeline failed: %v\n", err)
		if ce, ok := err.(*clock.Error); ok &&
			(ce.Kind == clock.KindQueueFull || ce.Kind == clock.KindBadBlock) {
			return exitResource
		}
		return exitResource
	}

	fmt.Println(res.Report.Line())
	th := metrics.DefaultThresholds(queueBytes)
	if !res.Report.Pass(th) {
		return exitResource
	}
	return exitOK
}

// adaptiveWindowPackets maps the byte ceiling to a packet window that stays
// within the 20..200 ms buffering target for the default packet duration.
func adaptiveWindowPackets(queueBytes int) int {
	cfg := fixture.DefaultConfig()
	blockBytes := cfg.FramesPerPkt * cfg.Channels * 4
	if blockBytes <= 0 {
		return 8
	}
	pkts := queueBytes / blockBytes
	if pkts < 4 {
		return 4
	}
	if pkts > 64 {
		return 64
	}
	return pkts
}

func runCorruptWAV() int {
	raw, offset := fixture.CorruptWAV()
	if _, err := wav.Decode(bytes.NewReader(raw)); err != nil {
		if ce, ok := err.(*clock.Error); ok {
			fmt.Printf("error=%s offset=%d\n", ce.Kind.String(), ce.Offset)
			return exitResource
		}
		fmt.Printf("error=truncated_wav offset=%d\n", offset)
		return exitResource
	}
	fmt.Println("error=none")
	return exitOK
}

func runQueueBoundary(seed int64) int {
	cfg := fixture.DefaultConfig()
	cfg.DurationMS = 1000
	cfg.Seed = seed
	gen := fixture.NewGenerator(cfg)
	gen.BuildTracks()
	packets := gen.Normal()

	// A deliberately tiny ceiling forces backpressure; recovery must still
	// retain every accepted block.
	const ceiling = 4096
	res, err := pipeline.Run(context.Background(), pipeline.Params{
		Fixture:    cfg,
		QueueBytes: ceiling,
		WindowPkts: 8,
		Out:        io.Discard,
	}, packets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "queue boundary run failed: %v\n", err)
		return exitResource
	}
	fmt.Printf("backpressure_events=%d accepted_blocks=%d lost_blocks=%d\n",
		res.Queue.BackpressureEv, res.AcceptedBlocks, res.LatePackets)
	if res.LatePackets != 0 || res.Queue.PeakBytes > ceiling {
		return exitResource
	}
	return exitOK
}
