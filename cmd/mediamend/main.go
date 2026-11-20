// Command mediamend drives the deterministic local media-repair pipeline from
// the terminal. Subcommands:
//
//	demo    generate fixtures and run (optionally interrupting at a stage)
//	resume  continue an interrupted/finished run using its checkpoint
//	verify  re-verify random seek points against the generated indexes
//
// Exit codes: 0 success, 2 corrupt input / verification failure,
// 3 dependency or graph error, 4 resume-state mismatch.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/mediamend/mediamend/internal/checkpoint"
	"github.com/mediamend/mediamend/internal/fixture"
	"github.com/mediamend/mediamend/internal/index"
	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/mediamend/mediamend/internal/pipeline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "resume":
		err = runResume(os.Args[2:], true)
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		if fe, ok := err.(*model.FaultError); ok {
			os.Exit(model.ExitCode(fe.Faults))
		}
		if err == pipeline.ErrInterrupted {
			os.Exit(0) // controlled interrupt; checkpoint persisted
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediamend - deterministic local multi-track media repair

usage:
  mediamend demo   --root DIR [--corrupt-every N] [--interrupt-after STAGE]
                   [--frames N] [--random-seeks N] [--seed N]
  mediamend resume --root DIR [--json]
  mediamend verify --root DIR [--random-seeks N] [--seed N]

stages: ingest, timeline, graph, audio, color, index, manifest
exit codes: 0 ok | 2 corrupt/verify | 3 graph/dependency | 4 resume mismatch
`)
}

type commonFlags struct {
	root         string
	corruptEvery uint64
	interrupt    string
	frames       uint64
	randomSeeks  int
	seed         int64
	asJSON       bool
}

func parseFlags(args []string, fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{randomSeeks: 200, frames: fixture.DefaultFramesPerTrack, seed: 1}
	fs.StringVar(&c.root, "root", "", "run directory (required)")
	fs.Uint64Var(&c.corruptEvery, "corrupt-every", 97, "corrupt every Nth frame (0 disables)")
	fs.StringVar(&c.interrupt, "interrupt-after", "", "stop after this stage")
	fs.Uint64Var(&c.frames, "frames", fixture.DefaultFramesPerTrack, "frames per track")
	fs.IntVar(&c.randomSeeks, "random-seeks", 200, "random seek checks per track")
	fs.Int64Var(&c.seed, "seed", 1, "RNG seed")
	_ = fs.Parse(args)
	return c
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	c := parseFlags(args, fs)
	if c.root == "" {
		return fmt.Errorf("--root is required")
	}
	cfg, err := buildConfig(c, false)
	if err != nil {
		return err
	}
	cfg.InterruptAfter = c.interrupt
	start := time.Now()
	res, err := pipeline.Run(cfg)
	res.ResumeMS = time.Since(start).Milliseconds()
	if err == pipeline.ErrInterrupted {
		fmt.Fprintf(os.Stderr, "interrupted after stage %q; checkpoint saved at %s\n",
			c.interrupt, filepath.Join(c.root, checkpoint.FileName))
		printJSON(res)
		return pipeline.ErrInterrupted
	}
	if err != nil {
		printJSON(res)
		return err
	}
	finalizeAndPrint(c, res)
	return nil
}

func runResume(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	c := &commonFlags{randomSeeks: 200, frames: fixture.DefaultFramesPerTrack, seed: 1}
	fs.StringVar(&c.root, "root", "", "run directory (required)")
	fs.IntVar(&c.randomSeeks, "random-seeks", 200, "random seek checks per track")
	fs.Int64Var(&c.seed, "seed", 1, "RNG seed")
	fs.BoolVar(&c.asJSON, "json", false, "emit JSON manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.root == "" {
		return fmt.Errorf("--root is required")
	}
	cfg, err := buildConfig(c, true)
	if err != nil {
		return err
	}
	start := time.Now()
	res, err := pipeline.Run(cfg)
	res.ResumeMS = time.Since(start).Milliseconds()
	if err != nil {
		printJSON(res)
		return err
	}
	if asJSON || c.asJSON {
		printJSON(res)
	} else {
		finalizeAndPrint(c, res)
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	c := parseFlags(args, fs)
	if c.root == "" {
		return fmt.Errorf("--root is required")
	}
	proj, err := loadProject(c.root)
	if err != nil {
		return err
	}
	var allFaults []model.Fault
	totalChecked := 0
	for _, spec := range proj.Tracks {
		idxPath := filepath.Join(c.root, "output", "index-"+spec.ID+".mp4")
		idx := loadIndex(idxPath, spec.ID)
		if idx == nil {
			allFaults = append(allFaults, model.Fault{
				Class: model.FaultIndexBounds, Severity: model.SeverityFatal,
				Location: model.SourceLocation{File: idxPath, Track: spec.ID},
				Message:  "index file missing or unreadable",
			})
			continue
		}
		vf := index.Verify(idxPath, idx)
		allFaults = append(allFaults, vf...)
		rng := rand.New(rand.NewSource(c.seed))
		checked, sf := index.RandomSeeks(idx, c.randomSeeks, rng)
		totalChecked += checked
		allFaults = append(allFaults, sf...)
	}
	out := map[string]any{
		"seek_checked": totalChecked,
		"random_seeks": c.randomSeeks,
		"fault_count":  len(allFaults),
	}
	if model.HasFatal(allFaults) {
		out["faults"] = allFaults
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return &model.FaultError{Faults: allFaults}
	}
	out["status"] = "ok"
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	return nil
}

// buildConfig regenerates the deterministic fixture and assembles the pipeline
// config. When preferSaved is true (resume/verify), it restores the exact
// generation parameters persisted in fixture.json so the rebuilt dataset is
// byte-identical to the original run and its checkpoint can be reused; a real
// change to the inputs yields a different digest and is rejected (exit 4).
func buildConfig(c *commonFlags, preferSaved bool) (pipeline.Config, error) {
	opts := fixture.Options{
		Root: c.root, FramesPerTrack: c.frames, CorruptEvery: c.corruptEvery,
		Seed:           c.seed,
		InjectRollover: true, InjectDuplicate: true, InjectBackwards: true,
		InjectMissing: true, InjectColorFault: true,
	}
	if preferSaved {
		if saved, ok, err := fixture.LoadOptions(c.root); err != nil {
			return pipeline.Config{}, err
		} else if ok {
			saved.Root = c.root // honor the current run directory
			opts = saved
		}
	}
	proj, corrupt, err := fixture.Generate(opts)
	if err != nil {
		return pipeline.Config{}, err
	}
	var inputs []ingest.TrackInput
	for _, s := range proj.Tracks {
		inputs = append(inputs, ingest.TrackInput{TrackID: s.ID, File: s.File})
	}
	return pipeline.Config{
		Root: c.root, Project: proj, Inputs: inputs,
		KeyEvery: proj.KeyEvery, MaxWorkers: 4,
		RandomSeeks: c.randomSeeks, SeekSeed: c.seed,
		ExpectedCorrupt: corrupt,
	}, nil
}

func finalizeAndPrint(c *commonFlags, res *pipeline.Result) {
	printJSON(res)
}

func printJSON(res *pipeline.Result) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}
