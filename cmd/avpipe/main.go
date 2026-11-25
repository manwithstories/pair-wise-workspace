// Command avpipe is the terminal-only driver for the media/RTP pipeline.
// It generates deterministic sample streams, runs every subsystem and prints
// the acceptance metrics plus an exit code:
//
//	0 = metrics pass
//	2 = input unrecoverable
//	3 = metrics below target
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"avpipe/internal/media"
	"avpipe/internal/metrics"
	"avpipe/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	frames := fs.Int("frames", 600, "number of image frames to generate")
	seed := fs.Int64("seed", 7, "deterministic RNG seed")
	loss := fs.Float64("loss", 0, "per-packet loss probability (0..1)")
	burst := fs.Int("burst", 0, "additional consecutive packets dropped per loss event")
	truncate := fs.Int("truncate", 0, "bytes removed from the container tail")
	corruptEvery := fs.Int("corrupt-every", 0, "corrupt every nth frame (0 disables)")
	showCase := fs.String("case", "", "named scenario: clean")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg := pipeline.DefaultConfig()
	cfg.Frames = *frames
	cfg.Seed = *seed
	cfg.LossRate = *loss
	cfg.Burst = *burst
	cfg.Truncate = *truncate
	cfg.CorruptEvery = *corruptEvery
	cfg.Case = *showCase
	if *showCase == "clean" {
		cfg.LossRate = 0
		cfg.Burst = 0
		cfg.Truncate = 0
		cfg.CorruptEvery = 0
	}

	rep, err := pipeline.NewEngine(cfg).Run(context.Background())
	if err != nil {
		if me, ok := err.(*media.Error); ok && me.Action == media.ActionFail {
			fmt.Fprintf(os.Stderr, "unrecoverable input: %v\n", err)
			return 2
		}
		fmt.Fprintf(os.Stderr, "pipeline error: %v\n", err)
		return 2
	}

	damaged := *showCase != "clean" &&
		(*loss > 0 || *truncate > 0 || *corruptEvery > 0)
	printReport(rep)
	verdict, reasons := rep.Evaluate(metrics.DefaultThresholds(), damaged)
	for _, r := range reasons {
		fmt.Fprintf(os.Stderr, "threshold fail: %s\n", r)
	}
	return verdict
}

func printReport(r *metrics.Report) {
	fmt.Printf("frames=%d\n", r.Frames)
	fmt.Printf("recovered_pct=%.2f\n", r.RecoveredPct)
	fmt.Printf("sync_max_ms=%.2f\n", r.SyncMaxMillis)
	fmt.Printf("latency_p95_ms=%.2f\n", r.LatencyP95Ms)
	fmt.Printf("bad_records=%d\n", r.BadRecords)
	fmt.Printf("index_fallback=%t\n", r.IndexFallback)
	fmt.Printf("output_sha256=%s\n", r.OutputSHA256)
	fmt.Printf("lost_packets=%d lost_media=%d recovered_fec=%d unrecoverable=%d overflows=%d late_drops=%d audio_gaps=%d parser_errors=%d\n",
		r.LostPackets, r.LostMedia, r.RecoveredFEC, r.Unrecoverable, r.BufferOverflows,
		r.LateDrops, r.AudioGaps, r.ParserErrors)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: avpipe demo [flags]")
	fmt.Fprintln(os.Stderr, "flags:")
	fmt.Fprintln(os.Stderr, "  --frames N          number of frames (default 600)")
	fmt.Fprintln(os.Stderr, "  --seed N            deterministic seed (default 7)")
	fmt.Fprintln(os.Stderr, "  --loss F            packet loss probability 0..1")
	fmt.Fprintln(os.Stderr, "  --burst N           burst length added to each loss")
	fmt.Fprintln(os.Stderr, "  --truncate N        bytes cut from the container tail")
	fmt.Fprintln(os.Stderr, "  --corrupt-every N   corrupt every Nth frame")
	fmt.Fprintln(os.Stderr, "  --case clean        force a defect-free run")
}
