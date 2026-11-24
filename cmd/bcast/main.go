// Command bcast is the acceptance and demonstration entry point for the
// broadcast delivery pipeline. It generates sample material in code, runs the
// demux → index → cut → silence → fingerprint → remux graph, and prints a
// stable key=value report.
//
// Exit codes:
//
//	0  processing and verification complete
//	2  input unrecoverable or a dependency is blocked
//	1  parameter, I/O or internal error
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"bcast/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return cmdDemo(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "bcast: unknown command %q\n", args[0])
		usage()
		return exitUsage
	}
}

const (
	exitOK      = 0
	exitError   = 1
	exitBlocked = 2
	exitUsage   = 1
)

func usage() {
	fmt.Fprint(os.Stderr, `bcast — broadcast media delivery demo

usage:
  bcast demo   --seed N --segments N --damage LIST --checkpoint PATH [--out DIR]
  bcast verify --checkpoint PATH

flags (demo):
  --seed        generator seed (default 17)
  --segments    number of 1s segments (default 12)
  --damage      comma list: truncate,broken-box,missing-index (default all)
  --checkpoint  checkpoint JSON path (required to verify later)
  --out         output directory (default: checkpoint dir/out)
  --window      silence window in audio units (default 4800)
  --threshold   silence RMS threshold (default 500)
  --workers     concurrent DAG workers (default 4)
`)
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	p := pipeline.DefaultParams()
	var seed, segs, window, workers int64
	var damage, checkpoint, out string
	var threshold float64
	fs.Int64Var(&seed, "seed", p.Seed, "generator seed")
	fs.Int64Var(&segs, "segments", int64(p.Segments), "segment count")
	fs.StringVar(&damage, "damage", "truncate,broken-box,missing-index", "damage list")
	fs.StringVar(&checkpoint, "checkpoint", "", "checkpoint JSON path")
	fs.StringVar(&out, "out", "", "output directory")
	fs.Int64Var(&window, "window", p.WindowSize, "silence window")
	fs.Float64Var(&threshold, "threshold", p.Threshold, "silence RMS threshold")
	fs.Int64Var(&workers, "workers", int64(p.Workers), "DAG workers")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	p.Seed = seed
	p.Segments = int(segs)
	p.WindowSize = window
	p.Threshold = threshold
	p.Workers = int(workers)
	if checkpoint == "" {
		fmt.Fprintln(os.Stderr, "bcast demo: --checkpoint is required")
		return exitUsage
	}
	dmgs, err := pipeline.ParseDamage(damage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcast demo: %v\n", err)
		return exitUsage
	}
	p.Damage = dmgs
	if out == "" {
		out = defaultOutDir(checkpoint)
	}
	p.OutDir = out
	// A corrupt checkpoint is discarded, not reused: the run is reproducible
	// from its parameters, so starting clean is always safe.
	cp, err := pipeline.LoadCheckpoint(checkpoint)
	if err != nil {
		if errors.Is(err, pipeline.ErrCheckpointCorrupt) {
			fmt.Fprintf(os.Stderr, "bcast demo: discarding corrupt checkpoint: %v\n", err)
			cp = pipeline.NewCheckpoint(checkpoint)
		} else {
			fmt.Fprintf(os.Stderr, "bcast demo: %v\n", err)
			return exitError
		}
	}
	sum, err := pipeline.Run(context.Background(), p, cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcast demo: %v\n", err)
		return exitError
	}
	fmt.Print(sum.Encode())
	// The primary deliverable determines the exit code. Expected damage
	// variants and the deliberately corrupt input are quarantined and
	// reported, not treated as a failed batch: the acceptance command runs
	// with damage injected and must still exit 0.
	if sum.PrimaryOK {
		return exitOK
	}
	return exitBlocked
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var checkpoint string
	fs.StringVar(&checkpoint, "checkpoint", "", "checkpoint JSON path")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if checkpoint == "" {
		fmt.Fprintln(os.Stderr, "bcast verify: --checkpoint is required")
		return exitUsage
	}
	cp, err := pipeline.LoadCheckpoint(checkpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcast verify: %v\n", err)
		return exitError
	}
	v, err := pipeline.Verify(cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcast verify: %v\n", err)
		return exitBlocked
	}
	fmt.Print(v.SummaryLine())
	return exitOK
}

// defaultOutDir keeps artefacts next to the checkpoint so a verify run can
// find them from the checkpoint path alone.
func defaultOutDir(checkpoint string) string {
	dir := strings.TrimSuffix(checkpoint, ".json")
	if dir == checkpoint || dir == "" {
		dir = checkpoint + ".out"
		return dir
	}
	return dir + "-out"
}
