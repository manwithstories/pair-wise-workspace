// Command mediamap is the acceptance entry point: it generates damaged
// multi-track HLS fixtures, recovers segment boundaries and timestamps,
// aligns the bitrate ladder, classifies RTP loss, and writes a verifiable
// JSON manifest plus diagnostic summary.
//
// Usage:
//
//	mediamap demo   --tracks 4 --ladder 3 --drop-burst 12 \
//	    --truncate-every 7 --seed 41 --json [--out generated/case.json]
//	mediamap verify --input generated/case.json
//
// Exit codes: 0 clean, 2 recoverable damage marked, 1 parameter error or
// unrecoverable structural error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediamap/internal/pipeline"
	"mediamap/internal/testkit"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	switch args[0] {
	case "demo":
		return demo(args[1:])
	case "verify":
		return verify(args[1:])
	default:
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mediamap demo [flags] | mediamap verify --input <case.json>")
}

func demo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	opts := testkit.Options{}
	fs.IntVar(&opts.Tracks, "tracks", 4, "number of isolated tracks")
	fs.IntVar(&opts.Ladder, "ladder", 3, "representations per track")
	fs.IntVar(&opts.DropBurst, "drop-burst", 12, "RTP burst loss length")
	fs.IntVar(&opts.TruncateEvery, "truncate-every", 7, "corrupt every Nth segment")
	seed := fs.Uint64("seed", 41, "deterministic fixture seed")
	asJSON := fs.Bool("json", false, "print full JSON to stdout")
	out := fs.String("out", filepath.Join("generated", "case.json"), "output manifest path")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if opts.Tracks < 1 || opts.Ladder < 1 || opts.DropBurst < 0 || opts.TruncateEvery < 2 {
		fmt.Fprintln(os.Stderr, "invalid parameters: need tracks>=1, ladder>=1, drop-burst>=0, truncate-every>=2")
		return 1
	}
	opts.Seed = *seed
	result, code := pipeline.RunDemo(opts)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal output:", err)
		return 1
	}
	if dir := filepath.Dir(*out); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "create output dir:", err)
			return 1
		}
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write output:", err)
		return 1
	}
	if *asJSON {
		fmt.Println(string(data))
	} else {
		s := result.Summary
		fmt.Printf("av_sync_ms=%.2f packet_recovery_pct=%.1f p95_latency_ms=%.2f "+
			"alignment_error_ms=%.2f corrupt_segments=%d manifest_hash=%s\n",
			s.AVSyncMs, s.PacketRecoveryPct, s.P95LatencyMs,
			s.AlignmentErrorMs, s.CorruptSegments, s.ManifestHash[:16])
		fmt.Printf("faults=%d output=%s\n", len(result.Faults), *out)
	}
	return code
}

func verify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	input := fs.String("input", "", "output manifest to re-check")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, "verify: --input is required")
		return 1
	}
	problems, err := pipeline.VerifyFile(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return 1
	}
	for _, p := range problems {
		fmt.Println("FAIL:", p)
	}
	if len(problems) > 0 {
		return 1
	}
	fmt.Println("OK:", *input)
	return 0
}
