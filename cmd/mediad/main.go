// Command mediad runs the media-delivery demo pipeline from the terminal.
//
// Usage:
//
//	mediad demo --seed 42 --duration 120 --loss-burst 8 --rates 3 --json
//
// Exit codes: 0 = samples processed, 2 = parameter error,
// 3 = unrecovered corrupt ranges remain.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"mediad/internal/pipeline"
	"mediad/internal/report"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediad demo [--seed N] [--duration S] [--loss-burst N] [--rates N] [--truncate] [--bad-audio] [--unrecoverable] [--json]")
		return 2
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	duration := fs.Int("duration", 120, "sample duration in seconds")
	lossBurst := fs.Int("loss-burst", 8, "injected burst packet loss")
	rates := fs.Int("rates", 3, "number of bitrate representation layers")
	truncate := fs.Bool("truncate", false, "inject a truncated elementary stream")
	badAudio := fs.Bool("bad-audio", false, "inject a wrong channel map and sample rate")
	unrecoverable := fs.Bool("unrecoverable", false, "drop lost packets from the sender buffer")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	cfg := pipeline.Config{
		Seed: *seed, DurationSec: *duration, LossBurst: *lossBurst, Rates: *rates,
		InjectTruncate: *truncate, BadAudio: *badAudio, Unrecoverable: *unrecoverable,
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "parameter error:", err)
		return 2
	}

	m, err := pipeline.Run(cfg)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if jerr := enc.Encode(m); jerr != nil {
			fmt.Fprintln(os.Stderr, "encode report:", jerr)
			return 1
		}
	} else {
		printText(m)
	}
	if errors.Is(err, pipeline.ErrUnrecovered) {
		fmt.Fprintf(os.Stderr, "error: %d unrecovered corrupt range(s) remain\n", m.UnrecoveredRanges)
		return 3
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func printText(m report.Metrics) {
	fmt.Printf("sync_error_ms:        %.2f\n", m.SyncErrorMS)
	fmt.Printf("loss_recovery_rate:   %.3f\n", m.LossRecoveryRate)
	fmt.Printf("index_entries:        %d\n", m.IndexEntries)
	fmt.Printf("boundary_mismatches:  %d\n", m.BoundaryMismatches)
	fmt.Printf("p95_latency_ms:       %.1f\n", m.P95LatencyMS)
	fmt.Printf("recovered_segments:   %d\n", m.RecoveredSegments)
	fmt.Printf("corrupted_ranges:     %d\n", m.CorruptedRanges)
	fmt.Printf("unrecovered_ranges:   %d\n", m.UnrecoveredRanges)
	fmt.Printf("index_digest:         %s\n", m.IndexDigest)
	for _, f := range m.Faults {
		fmt.Println("fault:", f)
	}
}
