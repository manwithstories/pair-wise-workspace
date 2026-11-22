// Command avscan is the acceptance entry point for the offline multi-track
// media review scanner. It runs entirely locally with no service dependency.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"avscan/internal/fixture"
	"avscan/internal/model"
	"avscan/internal/scan"
	"avscan/internal/timeline"
)

// demoOutput is the terminal/JSON contract exercised by the acceptance run.
type demoOutput struct {
	Tracks            int                     `json:"tracks"`
	Samples           int                     `json:"samples"`
	DamageSegments    int                     `json:"damage_segments"`
	FirstBadOffset    int64                   `json:"first_bad_offset"`
	CheckpointBlocks  int                     `json:"checkpoint_blocks"`
	ResumeMS          int64                   `json:"resume_ms"`
	IndexSHA256       string                  `json:"index_sha256"`
	RoundingReports   int                     `json:"clip_rounding_points"`
	KeyFrames         []scan.KeyFrameReport   `json:"key_frames,omitempty"`
	Colour            []timeline.ColourReport `json:"colour_reports,omitempty"`
	Audio             []timeline.AudioTrackReport `json:"audio_reports,omitempty"`
	Validation        map[string]string       `json:"validation,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(int(model.ExitCode(model.ErrInvalidArgs)))
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(int(model.ExitCode(model.ErrInvalidArgs)))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr,
		"usage: avscan demo --out <index.json> [--block-samples N] [--resume] [--workdir DIR]")
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("out", "", "output index path")
	blockSamples := fs.Int("block-samples", 64, "samples per processing block")
	resume := fs.Bool("resume", false, "resume from an existing checkpoint")
	workdir := fs.String("workdir", "", "fixture directory (defaults to temp)")
	if err := fs.Parse(args); err != nil {
		return int(model.ExitCode(model.ErrInvalidArgs))
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "--out is required")
		return int(model.ExitCode(model.ErrInvalidArgs))
	}
	if err := os.MkdirAll(filepathDir(*out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create out dir: %v\n", err)
		return int(model.ExitCode(model.ErrIndexWrite))
	}
	// Default the fixture directory to a stable sibling of the index so a
	// later --resume re-reads the identical input the checkpoint was built
	// from. A throwaway temp dir would change identity between processes.
	dir := *workdir
	if dir == "" {
		dir = filepath.Join(filepathDir(*out), ".avscan-fixtures")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "workdir: %v\n", err)
		return int(model.ExitCode(model.ErrIndexWrite))
	}

	set, err := fixture.WriteDemoSet(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate fixtures: %v\n", err)
		return int(model.ExitCode(model.ErrDamagedInput))
	}
	var start time.Time
	if *resume {
		start = time.Now()
	}
	outcome, err := analyze(set, *out, *blockSamples, *resume)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
		return int(model.ExitCode(err))
	}
	if *resume {
		outcome.ResumeMS = time.Since(start).Milliseconds()
	}
	raw, _ := json.MarshalIndent(outcome, "", "  ")
	fmt.Println(string(raw))
	if err := validate(outcome); err != nil {
		fmt.Fprintf(os.Stderr, "validation failed: %v\n", err)
		return int(model.ExitCode(model.ErrDamagedInput))
	}
	return 0
}
