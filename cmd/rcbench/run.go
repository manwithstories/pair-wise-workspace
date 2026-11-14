package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"rcbench/internal/job"
	"rcbench/internal/rate"
	"rcbench/internal/report"
	"rcbench/internal/source"
)

// safePath resolves p and rejects paths escaping the working directory.
func safePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the working directory", p)
	}
	return abs, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type encodeFlags struct {
	input         string
	width, height int
	fps           int
	targetBps     int64
	segmentFrames int
	workers       int
	out           string
	maxRetries    int
	failAt        int
	floor         float64
	spread        float64
	maxDev        float64
}

func parseEncodeFlags(fs *flag.FlagSet, f *encodeFlags) {
	fs.StringVar(&f.input, "input", "", "input .yuv path")
	fs.IntVar(&f.width, "width", 640, "frame width")
	fs.IntVar(&f.height, "height", 360, "frame height")
	fs.IntVar(&f.fps, "fps", 30, "frames per second")
	fs.Int64Var(&f.targetBps, "target-bps", 1200000, "target bitrate")
	fs.IntVar(&f.segmentFrames, "segment-frames", 30, "frames per segment")
	fs.IntVar(&f.workers, "workers", 4, "frame-level worker count")
	fs.StringVar(&f.out, "out", "", "job output directory")
	fs.IntVar(&f.maxRetries, "max-retries", 2, "retries per segment")
	fs.IntVar(&f.failAt, "fail-at-segment", 0, "inject failure at 1-based segment")
	fs.Float64Var(&f.floor, "psnr-floor", 0, "minimum per-segment PSNR dB")
	fs.Float64Var(&f.spread, "psnr-spread", 0, "maximum PSNR spread dB")
	fs.Float64Var(&f.maxDev, "max-deviation-pct", 8, "maximum bitrate deviation percent")
}

func (f *encodeFlags) validate() error {
	if f.input == "" || f.out == "" {
		return errors.New("--input and --out are required")
	}
	if f.fps <= 0 || f.targetBps <= 0 || f.segmentFrames <= 0 || f.workers < 1 || f.maxRetries < 0 {
		return errors.New("fps/target-bps/segment-frames/workers must be positive, max-retries >= 0")
	}
	return nil
}

func cmdEncode(args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("encode", stderr)
	var f encodeFlags
	parseEncodeFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		return report.ExitUsage, err
	}
	if err := f.validate(); err != nil {
		return report.ExitUsage, err
	}
	inPath, err := safePath(f.input)
	if err != nil {
		return report.ExitUsage, err
	}
	jobDir, err := safePath(f.out)
	if err != nil {
		return report.ExitUsage, err
	}
	if _, err := job.ReadMeta(jobDir); err == nil {
		return report.ExitUsage, fmt.Errorf("job directory %s already exists; use resume", f.out)
	}
	rdr, err := source.OpenReader(inPath, f.width, f.height)
	if err != nil {
		return report.ExitUsage, err
	}
	defer rdr.Close()
	sum, err := sha256File(inPath)
	if err != nil {
		return report.ExitUsage, err
	}
	segments := (rdr.Frames() + f.segmentFrames - 1) / f.segmentFrames
	meta := &job.Meta{
		Input: inPath, InputSHA256: sum, Width: f.width, Height: f.height,
		FPS: f.fps, TargetBps: f.targetBps, SegmentFrames: f.segmentFrames,
		TotalFrames: rdr.Frames(), Segments: segments, State: string(job.StatePending),
	}
	machine := job.NewMachine(job.StatePending)
	if err := machine.Transition(job.StateRunning); err != nil {
		return report.ExitUsage, err
	}
	meta.State = string(machine.State())
	if err := job.WriteMeta(jobDir, meta); err != nil {
		return report.ExitUsage, err
	}
	logger := log.New(stderr, "", 0)
	return runJob(jobDir, meta, rdr, machine, f.workers, f.maxRetries, f.failAt, f.floor, f.spread, f.maxDev, stdout, logger)
}

func cmdResume(args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("resume", stderr)
	jobDirFlag := fs.String("job", "", "job directory")
	workers := fs.Int("workers", 4, "frame-level worker count")
	floor := fs.Float64("psnr-floor", 0, "minimum per-segment PSNR dB")
	spread := fs.Float64("psnr-spread", 0, "maximum PSNR spread dB")
	maxDev := fs.Float64("max-deviation-pct", 8, "maximum bitrate deviation percent")
	if err := fs.Parse(args); err != nil {
		return report.ExitUsage, err
	}
	if *jobDirFlag == "" {
		return report.ExitUsage, errors.New("--job is required")
	}
	jobDir, err := safePath(*jobDirFlag)
	if err != nil {
		return report.ExitUsage, err
	}
	meta, err := job.ReadMeta(jobDir)
	if err != nil {
		return report.ExitUsage, err
	}
	state := job.State(meta.State)
	if state == job.StateCompleted {
		fmt.Fprintln(stdout, "job already completed")
		rep, err := report.Build(jobDir)
		if err != nil {
			return report.ExitUsage, err
		}
		rep.WriteText(stdout)
		code, _ := rep.Evaluate(*floor, *spread, *maxDev)
		return code, nil
	}
	if state == job.StateFailed {
		// give poisoned segments a fresh retry budget; if they fail
		// again the job ends failed and this command exits 5
		fmt.Fprintf(stdout, "job failed with poisoned segments %v; retrying them\n", meta.Poisoned)
		meta.Poisoned = nil
	}
	machine := job.NewMachine(state)
	if state == job.StateInterrupted || state == job.StatePending || state == job.StateFailed {
		if err := machine.Transition(job.StateRunning); err != nil {
			return report.ExitUsage, err
		}
	}
	// state running with no live process means the previous run died; resume directly
	meta.State = string(machine.State())
	if err := job.WriteMeta(jobDir, meta); err != nil {
		return report.ExitUsage, err
	}
	rdr, err := source.OpenReader(meta.Input, meta.Width, meta.Height)
	if err != nil {
		return report.ExitUsage, err
	}
	defer rdr.Close()
	sum, err := sha256File(meta.Input)
	if err != nil {
		return report.ExitUsage, err
	}
	if sum != meta.InputSHA256 {
		return report.ExitUsage, fmt.Errorf("input checksum mismatch: job expects %s, got %s", meta.InputSHA256, sum)
	}
	logger := log.New(stderr, "", 0)
	return runJob(jobDir, meta, rdr, machine, *workers, 2, 0, *floor, *spread, *maxDev, stdout, logger)
}

// runJob drives the scheduler and maps outcomes to exit codes.
func runJob(jobDir string, meta *job.Meta, rdr *source.Reader, machine *job.Machine,
	workers, maxRetries, failAt int, floor, spread, maxDev float64,
	stdout io.Writer, logger *log.Logger) (int, error) {

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctrl := rate.NewController(meta.TargetBps, meta.FPS, meta.SegmentFrames)
	opts := job.Options{Workers: workers, MaxRetries: maxRetries, FailAtSegment: failAt, Logger: logger}
	_, err := job.Run(ctx, jobDir, meta, rdr, ctrl, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if terr := machine.Transition(job.StateInterrupted); terr != nil {
				return report.ExitUsage, terr
			}
			meta.State = string(machine.State())
			if werr := job.WriteMeta(jobDir, meta); werr != nil {
				return report.ExitUsage, werr
			}
			fmt.Fprintln(stdout, "interrupted: checkpoints flushed, resume with `rcbench resume`")
			return report.ExitInterrupted, nil
		}
		if errors.Is(err, job.ErrPoisoned) {
			seg := extractPoisonSegment(meta, jobDir)
			meta.Poisoned = append(meta.Poisoned, seg)
			if terr := machine.Transition(job.StateFailed); terr != nil {
				return report.ExitUsage, terr
			}
			meta.State = string(machine.State())
			if werr := job.WriteMeta(jobDir, meta); werr != nil {
				return report.ExitUsage, werr
			}
			return report.ExitFailed, fmt.Errorf("segment %d poisoned; job marked failed", seg)
		}
		return report.ExitUsage, err
	}
	if err := machine.Transition(job.StateCompleted); err != nil {
		return report.ExitUsage, err
	}
	meta.State = string(machine.State())
	if err := job.WriteMeta(jobDir, meta); err != nil {
		return report.ExitUsage, err
	}
	rep, err := report.Build(jobDir)
	if err != nil {
		return report.ExitUsage, err
	}
	rep.WriteText(stdout)
	code, reasons := rep.Evaluate(floor, spread, maxDev)
	for _, r := range reasons {
		fmt.Fprintf(stdout, "threshold: %s\n", r)
	}
	if ctrl.VBVUnderflows() > 0 {
		fmt.Fprintf(stdout, "warning: vbv underflows: %d\n", ctrl.VBVUnderflows())
	}
	return code, nil
}

// extractPoisonSegment finds the first segment without a valid checkpoint.
func extractPoisonSegment(meta *job.Meta, jobDir string) int {
	for seg := 1; seg <= meta.Segments; seg++ {
		if _, err := job.ReadCheckpoint(jobDir, seg); err != nil {
			return seg
		}
	}
	return meta.Segments
}
