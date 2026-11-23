// Command mediacore is the terminal acceptance entry point. It generates local
// synthetic media, runs the full demux-to-remux pipeline and prints a single
// line of JSON. It never opens a network connection or decodes media.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"mediacore/internal/fixture"
	"mediacore/internal/pipeline"
)

// Exit codes per the acceptance contract.
const (
	exitOK              = 0
	exitUnrecoverable   = 1
	exitUsageOrInternal = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediacore demo [flags]")
		return exitUsageOrInternal
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic fixture seed")
	workers := fs.Int("workers", 4, "bounded worker pool size")
	variant := fs.Int("variant", 0, "container variant 0=isom/stco 1=iso5/co64")
	mode := fs.String("mode", "ok", "fixture damage mode")
	ckpt := fs.String("checkpoint", ".tmp/checkpoint.json", "checkpoint path")
	out := fs.String("out", ".tmp/out.mp4", "output container path")
	cancelAfter := fs.Duration("cancel-after", 0, "cancel after duration (e.g. 2s); 0 disables")
	blockWorkers := fs.Bool("block-workers", false,
		"hold workers until cancel (test harness for full-queue cancellation)")
	slowWorkers := fs.Duration("slow-workers", 0,
		"per-job worker delay so cancellation lands mid-processing (test harness)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsageOrInternal
	}
	if *workers < 1 || *workers > 64 {
		fmt.Fprintln(os.Stderr, "workers must be in [1,64]")
		return exitUsageOrInternal
	}

	cfg := pipeline.DefaultConfig(*seed, *workers, *ckpt, *out)
	cfg.Variant = *variant
	cfg.Mode = fixture.Mode(*mode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if *blockWorkers {
		gate := make(chan struct{})
		cfg.WorkerGate = gate
	}
	if *slowWorkers > 0 {
		cfg.WorkerDelay = *slowWorkers
	}
	if *cancelAfter > 0 {
		t := time.AfterFunc(*cancelAfter, cancel)
		defer t.Stop()
	}

	res, err := pipeline.Run(ctx, cfg)
	if err != nil {
		if se, ok := err.(*pipeline.StructuralError); ok {
			res.Status = "unrecoverable"
			res.FirstErrorOffset = se.Fault.Offset
			printJSON(res)
			return exitUnrecoverable
		}
		fmt.Fprintf(os.Stderr, "internal error: %v\n", err)
		return exitUsageOrInternal
	}
	printJSON(res)
	return exitOK
}

func printJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode error: %v\n", err)
		return
	}
	fmt.Println(string(b))
}
