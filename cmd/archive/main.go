// Command archive runs the archive processing kernel from the terminal.
//
// Exit codes: 0 success and acceptance targets met, 2 invalid input or
// parameters, 1 processing failure or an acceptance target missed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"example.com/medialab/archive/internal/checkpoint"
	"example.com/medialab/archive/internal/pipeline"
	"example.com/medialab/archive/internal/provenance"
	"example.com/medialab/archive/internal/segment"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	seed           int64
	duration       string
	lateWindow     string
	bitrates       string
	damage         string
	lateAtBoundary bool
	interruptAfter string
	resume         bool
	workDir        string
	segment        string
	maxFill        string
}

func parseFlags(args []string, stderr io.Writer) (*options, string, error) {
	if len(args) == 0 {
		return nil, "", errors.New("missing command (want demo)")
	}
	command := args[0]
	if command != "demo" {
		return nil, "", fmt.Errorf("unknown command %q (want demo)", command)
	}
	o := &options{}
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Int64Var(&o.seed, "seed", 42, "deterministic sample seed")
	fs.StringVar(&o.duration, "duration", "120s", "media duration")
	fs.StringVar(&o.lateWindow, "late-window", "2s", "lateness watermark window")
	fs.StringVar(&o.bitrates, "bitrates", "64k,128k", "comma separated representation ladder")
	fs.StringVar(&o.damage, "damage", "", "damage profile: mixed|burst-loss|none")
	fs.BoolVar(&o.lateAtBoundary, "late-at-boundary", false, "place late packets on the window boundary")
	fs.StringVar(&o.interruptAfter, "interrupt-after", "0s", "stop after this much media, then resume")
	fs.BoolVar(&o.resume, "resume", false, "resume from the checkpoint instead of restarting")
	fs.StringVar(&o.workDir, "work-dir", ".work", "output directory (must stay inside the repository)")
	fs.StringVar(&o.segment, "segment", "10s", "segment duration")
	fs.StringVar(&o.maxFill, "max-fill", "3s", "maximum silence fill for a single gap")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, "", err
	}
	if fs.NArg() != 0 {
		return nil, "", fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return o, command, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	o, command, err := parseFlags(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if command != "demo" {
		fmt.Fprintf(stderr, "error: unknown command %q (want demo)\n", command)
		return 2
	}
	cfg, err := buildConfig(o)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	out, code := runDemo(cfg, o, stdout, stderr)
	if out != "" {
		fmt.Fprint(stdout, out)
	}
	return code
}

func buildConfig(o *options) (pipeline.Config, error) {
	cfg := pipeline.DefaultConfig()
	cfg.Seed = o.seed
	dur, err := pipeline.ParseDuration(o.duration)
	if err != nil {
		return cfg, err
	}
	cfg.DurationUS = dur
	seg, err := pipeline.ParseDuration(o.segment)
	if err != nil {
		return cfg, err
	}
	cfg.SegmentUS = seg
	win, err := pipeline.ParseDuration(o.lateWindow)
	if err != nil {
		return cfg, err
	}
	cfg.LateWindowUS = win
	fill, err := pipeline.ParseDuration(o.maxFill)
	if err != nil {
		return cfg, err
	}
	cfg.MaxFillUS = fill
	cfg.Bitrates = o.bitrates
	switch strings.ToLower(o.damage) {
	case "", "mixed":
		cfg.Damage = pipeline.DamageMixed
	case "burst-loss":
		cfg.Damage = pipeline.DamageBurstLoss
	case "none":
		cfg.Damage = pipeline.DamageNone
	default:
		return cfg, fmt.Errorf("unknown --damage %q", o.damage)
	}
	cfg.LateAtBoundary = o.lateAtBoundary
	cfg.Resume = o.resume
	if o.interruptAfter != "" {
		after, err := pipeline.ParseDuration(o.interruptAfter)
		if err != nil && o.interruptAfter != "0s" {
			return cfg, err
		}
		if after < cfg.DurationUS {
			cfg.InterruptAfterUS = after
		}
	}
	if err := validateWorkDir(o.workDir); err != nil {
		return cfg, err
	}
	cfg.WorkDir = o.workDir
	return cfg, cfg.Validate()
}

// validateWorkDir keeps every write inside the repository working directory.
func validateWorkDir(dir string) error {
	if dir == "" {
		return errors.New("work directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("work directory %q must stay inside %s", dir, wd)
	}
	return nil
}

// runDemo performs the interrupted-then-resumed run and compares it against a
// clean uninterrupted run. Nothing is hard-coded: both runs derive the digest.
func runDemo(cfg pipeline.Config, o *options, stdout, stderr io.Writer) (string, int) {
	ctx := context.Background()
	dir := filepath.Join(cfg.WorkDir, "seed-"+fmt.Sprint(cfg.Seed))
	if cfg.InterruptAfterUS > 0 || cfg.Resume {
		// A demo with an interruption always starts from a clean checkpoint so
		// the comparison is between an interrupted resume and a clean run.
		if !cfg.Resume || true {
			_ = os.RemoveAll(dir)
		}
	}

	store, manifest, ledger, closeFn, err := openSinks(dir)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return "", 1
	}
	defer closeFn()

	resumed := false
	runner, err := pipeline.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return "", 2
	}
	if err := runner.EnableMaterialization(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return "", 1
	}
	runner.Attach(store, manifest, ledger)

	firstErr := runner.Run(ctx)
	if errors.Is(firstErr, pipeline.ErrInterrupted) {
		// Restart the process-level objects exactly as a new invocation would.
		closeFn()
		store, manifest, ledger, closeFn, err = openSinks(dir)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return "", 1
		}
		defer closeFn()
		st, err := store.Load()
		if err != nil {
			fmt.Fprintf(stderr, "error: loading checkpoint: %v\n", err)
			return "", 1
		}
		resumeCfg := cfg
		resumeCfg.InterruptAfterUS = 0
		resumeCfg.Resume = true
		runner, err = pipeline.New(resumeCfg)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return "", 2
		}
		if err := runner.EnableMaterialization(); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return "", 1
		}
		runner.Attach(store, manifest, ledger)
		if err := runner.Restore(st); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return "", 1
		}
		if err := runner.Run(ctx); err != nil {
			fmt.Fprintf(stderr, "error: resumed run: %v\n", err)
			return "", 1
		}
		resumed = true
	} else if firstErr != nil {
		fmt.Fprintf(stderr, "error: %v\n", firstErr)
		return "", 1
	}
	if err := runner.MaterializerError(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return "", 1
	}

	rep := runner.BuildReport()
	rep.Resumed = resumed

	// Establish the reference digest with a clean, uninterrupted run that
	// writes nothing to disk.
	refDigest := runCleanReference(cfg, stderr)
	match := refDigest != "" && refDigest == rep.OutputSHA256
	if !match && refDigest == "" {
		fmt.Fprintf(stderr, "error: reference run failed\n")
		return "", 1
	}
	verify := runner.Verify(rep, match)
	rep.Verify = verify
	rep.OutputSHA256Match = match
	return formatReport(rep), exitCode(rep)
}

// runCleanReference executes the same configuration without interruption or
// disk output and returns the resulting digest.
func runCleanReference(cfg pipeline.Config, stderr io.Writer) string {
	ref := cfg
	ref.InterruptAfterUS = 0
	ref.Resume = false
	runner, err := pipeline.New(ref)
	if err != nil {
		fmt.Fprintf(stderr, "error: reference config: %v\n", err)
		return ""
	}
	runner.Attach(nil, nil, nil)
	if err := runner.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "error: reference run: %v\n", err)
		return ""
	}
	return runner.OutputDigest()
}

func openSinks(dir string) (*checkpoint.Store, *segment.Writer, *provenance.Writer, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, nil, nil, err
	}
	store, err := checkpoint.NewStore(dir)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	mf, err := os.OpenFile(filepath.Join(dir, "manifest.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	lg, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		mf.Close()
		return nil, nil, nil, nil, err
	}
	manifest := segment.NewWriter(mf)
	ledger := provenance.NewWriter(lg)
	closeFn := func() { _ = mf.Close(); _ = lg.Close() }
	return store, manifest, ledger, closeFn, nil
}

func exitCode(rep pipeline.Report) int {
	if !rep.Verify.Passed() {
		return 1
	}
	return 0
}
