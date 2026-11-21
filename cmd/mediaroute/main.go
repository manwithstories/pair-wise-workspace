// Command mediaroute rebuilds playable timelines from sharded live
// distributions: manifest parsing, packet reordering, adaptive
// buffering, loss recovery, A/V sync, segment inspection and
// checkpointed resume.
package main

import (
	"flag"
	"fmt"
	"os"

	"mediaroute/internal/model"
)

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	if len(args) < 1 || args[0] != "demo" {
		fmt.Fprintln(stderr, "usage: mediaroute demo [--seed N] [--bandwidth SPEC] "+
			"[--drop-burst S:C] [--duplicate N] [--reorder N] [--corrupt-segment I] "+
			"[--expect-error] [--resume] [--checkpoint PATH]")
		return model.ExitInternal
	}
	p := defaultParams()
	var bw, drop string
	var resume, expectError bool
	var chkPath string
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Int64Var(&p.Seed, "seed", p.Seed, "deterministic input seed")
	fs.StringVar(&bw, "bandwidth", "2m,700k,3m", "bandwidth phases, e.g. 2m,700k,3m")
	fs.StringVar(&drop, "drop-burst", "18:6", "burst loss start:count")
	fs.IntVar(&p.Duplicates, "duplicate", 9, "number of duplicated packets")
	fs.IntVar(&p.Reorder, "reorder", 12, "number of reordered packet pairs")
	fs.IntVar(&p.CorruptSegment, "corrupt-segment", -1, "truncate this segment")
	fs.BoolVar(&expectError, "expect-error", false, "success means the expected error occurred")
	fs.BoolVar(&resume, "resume", false, "resume from checkpoint")
	fs.StringVar(&chkPath, "checkpoint", "/tmp/mediaroute.chk", "checkpoint path")
	if err := fs.Parse(args[1:]); err != nil {
		return model.ExitCorrupt
	}
	var err error
	if p.Bandwidth, err = parseBandwidth(bw); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return model.ExitCorrupt
	}
	if n, err := fmt.Sscanf(drop, "%d:%d", &p.DropStart, &p.DropCount); err != nil || n != 2 {
		fmt.Fprintln(stderr, "error: bad --drop-burst, want start:count")
		return model.ExitCorrupt
	}
	return runDemo(p, chkPath, resume, expectError)
}
