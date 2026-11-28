package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"presspack/internal/audit"
	"presspack/internal/core"
)

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int("seed", 1, "fixture generation seed")
	seconds := fs.Int("seconds", 12, "per-camera duration in seconds")
	segment := fs.Int("segment", 2, "segment length in seconds")
	targets := fs.String("targets", "400000,800000", "comma-separated bit rates (bps)")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if *out == "" || *seconds <= 0 || *segment <= 0 {
		fmt.Fprintln(os.Stderr, "demo: --out, positive --seconds and --segment are required")
		return exitFailure
	}
	rates, err := parseTargets(*targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		return exitFailure
	}
	if err := checkDeps(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		return exitFailure
	}
	srcDir := filepath.Join(*out, "src")
	delDir := filepath.Join(*out, "delivery")
	if err := generateFixtures(*seed, *seconds, srcDir); err != nil {
		fmt.Fprintln(os.Stderr, "demo: fixture generation:", err)
		return exitFailure
	}
	opts := pipeOptions{segLen: core.FromMillis(int64(*segment) * 1000), targets: rates}
	ctx := context.Background()
	rep, err := runPipeline(ctx, srcDir, delDir, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo: pipeline:", err)
		return exitFailure
	}
	// Safe rerun: completed variants resume, nothing re-segments; the
	// delivery hash must be identical.
	rep2, err := runPipeline(ctx, srcDir, delDir, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo: rerun:", err)
		return exitFailure
	}
	rep2.Seed, rep2.Seconds = *seed, *seconds
	repeat := "match"
	if rep.DeliveryHash != rep2.DeliveryHash {
		repeat = "mismatch"
	}
	metrics, err := computeMetrics(ctx, *out, rep2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo: metrics:", err)
		return exitFailure
	}
	metrics.RepeatHash = repeat
	if err := writeReport(*out, rep2); err != nil {
		fmt.Fprintln(os.Stderr, "demo: report:", err)
		return exitFailure
	}
	metrics.Print(os.Stdout)
	if rep2.partial() {
		fmt.Fprintln(os.Stdout, "status=partial_delivery")
		return exitPartial
	}
	if !metrics.Pass() {
		fmt.Fprintln(os.Stdout, "status=metrics_failed")
		return exitPartial
	}
	fmt.Fprintln(os.Stdout, "status=pass")
	return exitOK
}

func parseTargets(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("bad target bit rate %q", p)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets")
	}
	return out, nil
}

// computeMetrics aggregates the audit numbers over the delivered cameras.
func computeMetrics(ctx context.Context, root string, rep *Report) (audit.Metrics, error) {
	var m audit.Metrics
	delDir := filepath.Join(root, "delivery")
	srcDir := filepath.Join(root, "src")
	delivered := rep.delivered()
	m.Cameras = len(delivered)
	m.Variants = len(rep.Targets)
	m.RecoveredSpans = rep.recoveredSpans()
	targets := map[string]int{}
	for _, v := range variantNames(rep.Targets) {
		targets[v.name] = v.bitRate
	}
	var ssimSum float64
	var ssimN int
	for _, cam := range delivered {
		if cam.SegmentsPerVariant > m.SegmentsPerVariant {
			m.SegmentsPerVariant = cam.SegmentsPerVariant
		}
		if cam.MaxAlignMs > m.MaxAlignMs {
			m.MaxAlignMs = cam.MaxAlignMs
		}
		camOut := filepath.Join(delDir, cam.Name)
		dirs := map[string]string{}
		for name := range targets {
			dirs[name] = filepath.Join(camOut, name)
		}
		pct, err := audit.MeasureBitrateError(dirs, targets, core.Time{Ticks: cam.DurationTicks})
		if err != nil {
			return m, err
		}
		if pct > m.BitrateErrorPct {
			m.BitrateErrorPct = pct
		}
		srcMedia, err := findMedia(filepath.Join(srcDir, cam.Name))
		if err != nil {
			return m, err
		}
		for name := range targets {
			pl := filepath.Join(camOut, name, "index.m3u8")
			s, err := audit.MeasureSSIM(ctx, srcMedia, pl)
			if err != nil {
				return m, err
			}
			ssimSum += s
			ssimN++
			n, err := audit.CountDecodeErrors(ctx, pl)
			if err != nil {
				return m, err
			}
			m.DecodeErrors += n
		}
	}
	if ssimN > 0 {
		m.SSIMAvg = ssimSum / float64(ssimN)
	}
	return m, nil
}

type variantSpec struct {
	name    string
	bitRate int
}

func variantNames(targets []int) []variantSpec {
	out := make([]variantSpec, len(targets))
	for i, t := range targets {
		out[i] = variantSpec{name: fmt.Sprintf("v%d", t/1000), bitRate: t}
	}
	return out
}

func findMedia(camDir string) (string, error) {
	entries, err := os.ReadDir(camDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".mp4", ".ts", ".m4v", ".mov":
			return filepath.Join(camDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no media file in %s", camDir)
}

// runFF executes ffmpeg and returns combined output on failure.
func runFF(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}
