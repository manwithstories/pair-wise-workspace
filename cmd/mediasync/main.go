// mediasync is the demo entry point: it generates sample inputs, runs
// the pipeline library and prints acceptance data as JSON. Exit code 0
// means success, 2 means corrupt input was handled with reviewable
// defect records, 1 means an unrecoverable index or internal error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"mediasync/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) < 1 || argv[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediasync demo [flags]")
		return 1
	}
	cfg, err := parseFlags(argv[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediasync:", err)
		return 1
	}
	res, err := pipeline.Run(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediasync:", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintln(os.Stderr, "mediasync:", err)
		return 1
	}
	return res.ExitCode
}

func parseFlags(argv []string) (pipeline.Config, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic sample seed")
	packets := fs.Int("packets", 12000, "number of RTP packets to generate")
	chunk := fs.Int("chunk", 256, "packets per checkpointed chunk")
	lossBurst := fs.Int("loss-burst", 0, "injected burst loss length in packets")
	bandwidth := fs.String("bandwidth", "1.0", "comma-separated simulated bandwidth profile (MB/s)")
	corrupt := fs.String("corrupt", "", "inject corruption: subtitle|index|output")
	interruptAfter := fs.Int("interrupt-after", 0, "stop after N committed chunks (simulated crash)")
	resume := fs.Bool("resume", false, "resume from the last valid checkpoint")
	dir := fs.String("dir", "mediasync-out", "local output directory")
	if err := fs.Parse(argv); err != nil {
		return pipeline.Config{}, err
	}
	var bw []float64
	for _, s := range strings.Split(*bandwidth, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || v <= 0 {
			return pipeline.Config{}, fmt.Errorf("invalid bandwidth value %q", s)
		}
		bw = append(bw, v)
	}
	switch *corrupt {
	case "", "subtitle", "index", "output":
	default:
		return pipeline.Config{}, fmt.Errorf("unknown --corrupt %q", *corrupt)
	}
	return pipeline.Config{
		Seed: *seed, Packets: *packets, Chunk: *chunk, LossBurst: *lossBurst,
		Bandwidth: bw, Corrupt: *corrupt, InterruptAfter: *interruptAfter,
		Resume: *resume, Dir: *dir,
	}, nil
}
