package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"turncode/internal/config"
	"turncode/internal/fixture"
	"turncode/internal/pipeline"
	"turncode/internal/report"
	"turncode/internal/state"
)

const (
	exitOK          = 0
	exitIO          = 1
	exitValidation  = 2
	exitInterrupted = 3
	exitRegression  = 4
	exitState       = 5
)

func main() { os.Exit(runCLI(os.Args[1:])) }

func runCLI(args []string) int {
	if len(args) == 0 {
		usage()
		return exitIO
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "fixture":
		return cmdFixture(rest)
	case "run":
		return cmdRun(rest, false)
	case "resume":
		return cmdRun(rest, true)
	case "report":
		return cmdReport(rest)
	case "help", "-h", "--help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		return exitIO
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  transcode fixture --out DIR --width 320 --height 180 --fps 30 --frames 600 --scene mixed --seed 42
  transcode run --spec manifest.json --target-bps 1200000 --gop 30 --segment-frames 60 --work DIR [--qp-offset N] [--fail-after-gop N] [--cancel]
  transcode resume --work DIR
  transcode report --work DIR [--json] [--baseline report.json]`)
}

func cmdFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opt fixture.Options
	fs.StringVar(&opt.OutDir, "out", "", "output directory")
	fs.IntVar(&opt.Width, "width", 320, "frame width")
	fs.IntVar(&opt.Height, "height", 180, "frame height")
	fs.IntVar(&opt.FPS, "fps", 30, "frames per second")
	fs.IntVar(&opt.Frames, "frames", 600, "frame count")
	fs.StringVar(&opt.Scene, "scene", "mixed", "slow|gradient|grain|cut|mixed")
	fs.Int64Var(&opt.Seed, "seed", 42, "deterministic seed")
	if err := fs.Parse(args); err != nil || opt.OutDir == "" {
		return exitIO
	}
	if err := checkPath(opt.OutDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitIO
	}
	abs, err := filepath.Abs(opt.OutDir)
	if err != nil {
		return exitIO
	}
	opt.OutDir = abs
	spec, err := fixture.Generate(opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, config.ErrInvalidSpec) || errors.Is(err, config.ErrBudget) {
			return exitValidation
		}
		return exitIO
	}
	fmt.Printf("manifest: %s\n", filepath.Join(abs, "manifest.json"))
	fmt.Printf("yuv_file: %s\nframes: %d\nfile_bytes: %d\n", spec.YUVFile, spec.Frames, spec.FileBytes)
	return exitOK
}

func cmdRun(args []string, resume bool) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var specPath, work string
	var target, gop, segment int64
	var qpOffset, failAfter int
	var cancel bool
	fs.StringVar(&specPath, "spec", "", "fixture manifest")
	fs.StringVar(&work, "work", "", "checkpoint work directory")
	fs.Int64Var(&target, "target-bps", 0, "target bitrate")
	fs.Int64Var(&gop, "gop", 0, "GOP length")
	fs.Int64Var(&segment, "segment-frames", 0, "quality segment length")
	fs.IntVar(&qpOffset, "qp-offset", 0, "force a QP offset for quality comparison")
	fs.IntVar(&failAfter, "fail-after-gop", 0, "inject interruption after N complete GOPs")
	fs.BoolVar(&cancel, "cancel", false, "cancel at the first GOP boundary")
	if err := fs.Parse(args); err != nil || work == "" {
		return exitIO
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if (seen["target-bps"] && target <= 0) || (seen["gop"] && gop <= 0) || (seen["segment-frames"] && segment <= 0) {
		fmt.Fprintln(os.Stderr, "target-bps, gop and segment-frames must be positive")
		return exitValidation
	}
	if failAfter < 0 || qpOffset < -51 || qpOffset > 51 {
		fmt.Fprintln(os.Stderr, "fail-after-gop must be non-negative and qp-offset must be in [-51,51]")
		return exitValidation
	}
	if err := checkPath(work); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitIO
	}
	absWork, err := filepath.Abs(work)
	if err != nil {
		return exitIO
	}
	var job *pipeline.Job
	if resume {
		if specPath != "" || target != 0 || gop != 0 || segment != 0 || qpOffset != 0 || failAfter != 0 {
			fmt.Fprintln(os.Stderr, "resume uses the frozen run.json configuration")
			return exitIO
		}
		job, err = pipeline.LoadJob(absWork)
		if err != nil {
			return classifyError(err)
		}
		if err := checkPath(job.SourcePath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitState
		}
		job.CancelAfterGOP = 0
		if cancel {
			job.CancelAfterGOP = 1
		}
	} else {
		job, err = buildRunJob(specPath, absWork, target, gop, segment, qpOffset)
		if err != nil {
			return classifyError(err)
		}
		job.FailAfterGOP = failAfter
		if cancel {
			job.CancelAfterGOP = 1
		}
	}
	out, runErr := pipeline.RunJob(context.Background(), job)
	if runErr != nil {
		if errors.Is(runErr, pipeline.ErrInterrupted) {
			fmt.Fprintf(os.Stderr, "interrupted after GOP %d; resume with --work %s\n", out.CompletedGOPs, absWork)
			return exitInterrupted
		}
		return classifyError(runErr)
	}
	return printWorkReport(absWork, false, "")
}

func buildRunJob(specPath, work string, target, gop, segment int64, qpOffset int) (*pipeline.Job, error) {
	if specPath == "" {
		return nil, fmt.Errorf("--spec is required")
	}
	if err := checkPath(specPath); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	var spec config.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("%w: manifest JSON: %v", config.ErrInvalidSpec, err)
	}
	spec.Defaults()
	if target != 0 {
		spec.TargetBPS = target
	}
	if gop != 0 {
		spec.GOP = int(gop)
	}
	if segment != 0 {
		spec.SegmentFrames = int(segment)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	manifestDir := filepath.Dir(specPath)
	source := filepath.Join(manifestDir, spec.YUVFile)
	if err := checkPath(source); err != nil {
		return nil, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	if info.Size() != spec.FileBytes {
		return nil, fmt.Errorf("%w: manifest expects %d bytes, file has %d", config.ErrInvalidSpec, spec.FileBytes, info.Size())
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	return &pipeline.Job{
		Spec:       spec,
		SourcePath: absSource,
		WorkDir:    work,
		QPOffset:   qpOffset,
	}, nil
}

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var work, baseline string
	var asJSON bool
	fs.StringVar(&work, "work", "", "checkpoint work directory")
	fs.StringVar(&baseline, "baseline", "", "baseline report.json")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	if err := fs.Parse(args); err != nil || work == "" {
		return exitIO
	}
	if err := checkPath(work); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitIO
	}
	absWork, err := filepath.Abs(work)
	if err != nil {
		return exitIO
	}
	return printWorkReport(absWork, asJSON, baseline)
}

func printWorkReport(work string, asJSON bool, baselinePath string) int {
	meta, err := pipeline.LoadMeta(work)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return classifyError(err)
	}
	bsPath := filepath.Join(work, "bitstream.bin")
	info, err := os.Stat(bsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitIO
	}
	cps := state.NewCheckpointStore(work)
	cp, err := cps.Latest()
	if err != nil {
		rec, recErr := cps.Recover(info.Size(), meta.SpecHash)
		if recErr != nil {
			fmt.Fprintln(os.Stderr, "checkpoint unavailable:", err)
			return exitState
		}
		cp = rec.Checkpoint
	}
	rep := report.Build(report.Input{Spec: meta.Spec, BitstreamBytes: info.Size(), Checkpoint: cp})
	regression := false
	if baselinePath != "" {
		if err := checkPath(baselinePath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitIO
		}
		base, err := report.Load(baselinePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitIO
		}
		reg := report.ApplyBaseline(&rep, base)
		regression = reg.Regression
	}
	if err := report.WriteJSON(report.WorkReportPath(work), rep); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitIO
	}
	if asJSON {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitIO
		}
		fmt.Println(string(raw))
	} else if err := report.WriteText(os.Stdout, rep); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitIO
	}
	if regression {
		return exitRegression
	}
	return exitOK
}

func classifyError(err error) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, config.ErrInvalidSpec) || errors.Is(err, config.ErrBudget) {
		return exitValidation
	}
	if errors.Is(err, state.ErrInvalidTransition) || errors.Is(err, state.ErrCorruptJournal) ||
		errors.Is(err, state.ErrNoCheckpoint) || errors.Is(err, pipeline.ErrWorkConflict) {
		return exitState
	}
	return exitIO
}

// checkPath rejects parent traversal and any symlink component.
func checkPath(path string) error {
	if path == "" {
		return errors.New("empty path")
	}
	clean := filepath.Clean(path)
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts {
		if part == ".." {
			return fmt.Errorf("path traversal rejected: %s", path)
		}
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return err
	}
	parts = strings.Split(abs, string(filepath.Separator))
	cur := string(filepath.Separator)
	for _, part := range parts {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if !isSystemRootLink(cur) {
				return fmt.Errorf("symlink rejected: %s", cur)
			}
			target, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return err
			}
			cur = target
		}
	}
	return nil
}

// macOS exposes compatibility symlinks at the filesystem root. They are part
// of the platform, not user-controlled indirection, so they are canonicalized.
func isSystemRootLink(path string) bool {
	switch filepath.Clean(path) {
	case "/var", "/tmp", "/etc", "/bin", "/sbin":
		return true
	default:
		return false
	}
}
