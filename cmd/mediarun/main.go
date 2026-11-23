// Command mediarun is the acceptance and demonstration entry point for the
// recovery library. The library itself lives under internal/ and is reusable
// on its own; this command only parses flags, drives the pipeline and prints
// the invariant summary.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediarun/internal/clock"
	"mediarun/internal/manifest"
	"mediarun/internal/pipeline"
)

// Exit codes are part of the contract.
const (
	exitOK          = 0
	exitRejected    = 2
	exitUnrecover   = 3
	exitRecoveryErr = 4
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitRejected)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "validate":
		os.Exit(runValidate(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(exitRejected)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `mediarun - media recovery and transaction demo

Usage:
  mediarun demo --seed N [--fault reorder,truncate,drift] [--resume] [--workdir DIR]
  mediarun validate --playlist FILE [--limits N]

Exit codes:
  0 all invariants hold
  2 input or manifest rejected
  3 timeline unrecoverable
  4 recovery or hash verification failed
`)
}

// demoFlags holds the parsed demo command line.
type demoFlags struct {
	seed     int64
	faults   pipeline.Faults
	resume   bool
	workdir  string
	segments int
	durNS    int64
}

func parseDemoFlags(args []string) (demoFlags, error) {
	f := demoFlags{seed: 42, segments: 12, durNS: 4_000_000_000}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&f.seed, "seed", 42, "deterministic sample seed")
	faultStr := fs.String("fault", "", "comma-separated faults: reorder,truncate,drift,drop,wrap")
	fs.BoolVar(&f.resume, "resume", false, "generate and resume an interrupted run")
	fs.StringVar(&f.workdir, "workdir", "", "working directory (default: a temp dir)")
	fs.IntVar(&f.segments, "segments", 12, "segments per camera")
	fs.Int64Var(&f.durNS, "segment-duration", 4_000_000_000, "segment duration in ns")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	faults, err := pipeline.ParseFaults(*faultStr)
	if err != nil {
		return f, err
	}
	f.faults = faults
	return f, nil
}

// runDemo produces three samples — clean, damaged and interrupted-then-resumed
// — and prints the invariant summary for the recovered run.
func runDemo(args []string) int {
	f, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitRejected
	}
	root := f.workdir
	if root == "" {
		tmp, err := os.MkdirTemp("", "mediarun-demo-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitRecoveryErr
		}
		root = tmp
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitRecoveryErr
	}
	fmt.Printf("workdir=%s\n", root)

	runCfg := pipeline.Config{
		Seed: f.seed, Segments: f.segments, SegmentDurNS: f.durNS,
		PacketRate: 50, Cameras: []string{"cam0", "cam1", "cam2"},
	}

	// 1. Clean reference run.
	cleanCfg := runCfg
	cleanCfg.WorkDir = filepath.Join(root, "clean")
	clean, err := pipeline.Run(cleanCfg)
	if code := classify(err); code != exitOK {
		fmt.Fprintln(os.Stderr, "clean run failed:", err)
		return code
	}
	fmt.Printf("clean_hash=%s\n", clean.NormalHash)

	// 2. Damaged run with the requested faults, used as the full-rerun
	// reference for a resumed run.
	damagedCfg := runCfg
	damagedCfg.WorkDir = filepath.Join(root, "damaged")
	damagedCfg.Faults = f.faults
	damaged, err := pipeline.Run(damagedCfg)
	if code := classify(err); code != exitOK {
		fmt.Fprintln(os.Stderr, "damaged run failed:", err)
		return code
	}

	res := damaged
	if f.resume {
		// 3. Interrupted run followed by resume inside one work directory, so
		// the resume continues the interrupted run's durable checkpoints.
		// The interruption is placed at the sync stage so several checkpoints
		// are already committed.
		resumedCfg := damagedCfg
		resumedCfg.WorkDir = filepath.Join(root, "resume")
		resumedCfg.Inject = pipeline.InjectCommitCrash
		resumedCfg.InjectStage = "sync"
		if _, err := pipeline.Run(resumedCfg); err == nil {
			fmt.Fprintln(os.Stderr, "expected the interrupted run to fail")
			return exitRecoveryErr
		}
		resumedCfg.Inject = pipeline.InjectNone
		resumedCfg.InjectStage = ""
		resumedCfg.Resume = true
		resumedCfg.ReferenceHash = damaged.NormalHash
		res, err = pipeline.Run(resumedCfg)
		if code := classify(err); code != exitOK {
			fmt.Fprintln(os.Stderr, "resumed run failed:", err)
			return code
		}
	}

	check := pipeline.Invariants{
		Roundtrip:     res.RoundtripRate,
		KeyframeHit:   res.KeyframeHitRate,
		SegmentHashes: res.SegmentHashMatch,
		ResumeEqual:   res.ResumeHashEqual,
		ResumeChecked: res.ResumeChecked,
		Provenance:    res.ProvenanceComplete,
	}
	fmt.Print(res.FormatRates())
	if !f.resume {
		// Everything except the resume comparison was checked. The demo
		// reports the partial result instead of passing the full contract.
		fmt.Println("invariants=partial (resume_hash_equal not verified; pass --resume)")
		return exitOK
	}
	if !check.Pass() {
		fmt.Println("invariants=FAILED")
		return exitRecoveryErr
	}
	fmt.Println("invariants=ok")
	return exitOK
}

func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("playlist", "", "path to an HLS media playlist")
	limit := fs.Int("limits", 0, "maximum manifest entries")
	if err := fs.Parse(args); err != nil || *path == "" {
		fmt.Fprintln(os.Stderr, "validate requires --playlist FILE")
		return exitRejected
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitRejected
	}
	limits := manifest.DefaultLimits()
	if *limit > 0 {
		limits.MaxEntries = *limit
	}
	media, err := manifest.ParseHLSBytes(data, limits)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rejected:", err)
		return classify(err)
	}
	fmt.Printf("accepted kind=%s segments=%d target_ms=%d\n",
		media.Kind, len(media.Segments), media.TargetDurNS/1_000_000)
	return exitOK
}

// classify maps a typed error to the documented exit code.
func classify(err error) int {
	if err == nil {
		return exitOK
	}
	switch clock.KindOf(err) {
	case clock.KindInvalid:
		return exitRejected
	case clock.KindUnrecoverable:
		return exitUnrecover
	default:
		return exitRecoveryErr
	}
}
