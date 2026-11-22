// medpipe rebuilds multi-track timelines from framed RTP/RTCP packet
// streams and exports reviewable frame sequences with full provenance.
//
// Usage:
//
//	medpipe demo   --seed N --tracks N --packets N --loss F \
//	    --reorder-window N --workers N --checkpoint PATH --out DIR [--resume]
//	medpipe verify --out DIR
//
// Exit codes: 0 = verification passed, 2 = input corrupt but located,
// 1 = run or output validation failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"medpipe/internal/export"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "demo":
		return runDemo(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	default:
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: medpipe <demo|verify> [flags]")
}

func runDemo(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var p export.RunParams
	fs.Int64Var(&p.Seed, "seed", 1, "deterministic sample seed")
	fs.IntVar(&p.Tracks, "tracks", 3, "number of tracks")
	fs.IntVar(&p.Packets, "packets", 12000, "total RTP packets")
	fs.Float64Var(&p.Loss, "loss", 0.02, "packet loss rate")
	fs.IntVar(&p.ReorderWindow, "reorder-window", 64, "reorder window size")
	fs.IntVar(&p.Workers, "workers", 4, "decode worker count")
	fs.IntVar(&p.Queue, "queue", 256, "job queue capacity")
	fs.StringVar(&p.CheckpointPath, "checkpoint", "./tmp/checkpoint.json", "checkpoint path")
	fs.StringVar(&p.OutDir, "out", "./tmp/out", "output directory")
	fs.BoolVar(&p.Resume, "resume", false, "resume from checkpoint")
	fs.IntVar(&p.ClockJumpTrack, "clock-jump-track", -1, "inject RTCP clock jump on track (-1 disables)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	m, err := export.Run(ctx, p)
	if m != nil {
		printMetrics(m)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
		return 1
	}
	if m.ProvenanceErrors > 0 {
		// Input corruption was located and isolated; artifacts are
		// complete for the healthy tracks.
		return 2
	}
	return 0
}

func runVerify(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	out := fs.String("out", "./tmp/out", "output directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	m, err := export.Verify(ctx, *out)
	if m != nil {
		printMetrics(m)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		return 1
	}
	if m.ProvenanceErrors > 0 {
		return 2
	}
	return 0
}

func printMetrics(m *export.Metrics) {
	fmt.Printf("packets_total=%d\n", m.PacketsTotal)
	fmt.Printf("packets_lost=%d\n", m.PacketsLost)
	fmt.Printf("bad_offset_bytes=%d\n", m.BadOffsetBytes)
	fmt.Printf("frames_written=%d\n", m.FramesWritten)
	fmt.Printf("resume_seconds=%.3f\n", m.ResumeSeconds)
	fmt.Printf("output_sha256=%s\n", m.OutputSHA256)
	fmt.Printf("provenance_errors=%d\n", m.ProvenanceErrors)
}
