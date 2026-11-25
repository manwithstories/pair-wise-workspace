// Command mediad runs the terminal-only media repair and acceptance demo.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		printUsage(os.Stderr)
		return 4
	}
	fs := flag.NewFlagSet("mediad demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scenario := fs.String("scenario", "clean", "clean | damaged | missing-init")
	seed := fs.Int64("seed", 73, "deterministic fixture seed")
	out := fs.String("out", "", "local output directory")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return 0
		}
		return 4
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", fs.Args())
		return 4
	}

	res, err := pipeline.Run(pipeline.Options{Scenario: *scenario, Seed: *seed, Out: *out})
	if res != nil {
		printResult(res)
	}
	if err != nil {
		code := media.ExitCode(err)
		fmt.Fprintf(os.Stderr, "mediad: %v\n", err)
		fmt.Printf("exit_code=%d\n", code)
		return code
	}
	fmt.Println("exit_code=0")
	return 0
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: mediad demo --scenario <clean|damaged|missing-init> --seed <n> --out <dir>")
}

func printResult(res *pipeline.Result) {
	fmt.Printf("scenario=%s seed=%d\n", res.Options.Scenario, res.Options.Seed)
	if res.FirstBlocked != "" {
		fmt.Printf("segments=%d\n", len(res.Repair.Segments))
		fmt.Printf("first_blocked=%s\n", res.FirstBlocked)
		return
	}
	fmt.Printf("roundtrip=%.2f%%\n", res.RoundTripRate)
	fmt.Printf("keyframe_hit=%.2f%%\n", res.KeyframeRate)
	fmt.Printf("segments=%d\n", len(res.Repair.Segments))
	fmt.Printf("hash_match=%d/%d\n", res.HashMatched, res.HashTotal)
	fmt.Printf("duplicate=%d\n", res.Repair.Duplicates)
	fmt.Printf("burst_loss=%d\n", res.Repair.BurstLosses)
	fmt.Printf("bandwidth_steps=%d\n", res.Repair.BandwidthSteps)
	for _, h := range res.SegmentHashes {
		fmt.Printf("segment_hash global=%02d path=%s sha256=%s\n", h.GlobalIndex, h.RelPath, h.SHA256)
	}
	if res.DamageRejected != "" {
		fmt.Printf("damage_rejected=%q\n", res.DamageRejected)
	}
}
