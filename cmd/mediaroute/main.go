// Command mediaroute is the terminal entry point: it generates synthetic
// multi-rendition RTP/metadata streams, runs the reorder/align/manifest
// pipeline with checkpoint recovery, and verifies published output.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mediaroute/internal/fault"
	"mediaroute/internal/pipeline"
	"mediaroute/internal/sim"
)

const markerFile = ".mediaroute-out"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 1
	}
	switch args[0] {
	case "demo":
		return demo(args[1:])
	case "verify":
		return verifyCmd(args[1:])
	default:
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  mediaroute demo --seed N --loss-burst N --bandwidth 2m,800k,3m [--corrupt truncated-box] --out DIR")
	fmt.Fprintln(os.Stderr, "  mediaroute verify --dir DIR")
}

func demo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "deterministic generator seed")
	lossBurst := fs.Int("loss-burst", 0, "consecutive packets dropped on video-480p")
	bandwidth := fs.String("bandwidth", "2m", "comma-separated bandwidth steps (k/m suffix)")
	corrupt := fs.String("corrupt", "", "fault injection: truncated-box")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	bw, err := parseBandwidth(*bandwidth)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	dir, err := safeOutDir(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	stream := sim.Generate(sim.Options{
		Seed: *seed, LossBurst: *lossBurst, Bandwidth: bw, Corrupt: *corrupt,
	})
	start := time.Now()
	straight, err := pipeline.Run(stream, pipeline.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	elapsed := time.Since(start)
	// Second run: checkpointed, crashed at 60% and recovered.
	crashAt := len(stream.Items) * 3 / 5
	resumed, err := pipeline.Run(stream, pipeline.Config{
		CheckpointInterval: len(stream.Items) / 5,
		CrashAt:            crashAt,
		WorkDir:            dir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := pipeline.WriteOutput(dir, straight); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	_, lines, mismatches := pipeline.VerifyOutput(dir)
	for _, l := range lines {
		fmt.Println(l)
	}
	consistency := compareManifests(straight, resumed)
	items := len(stream.Items)
	pps := float64(items) / elapsed.Seconds()
	fmt.Printf("roundtrip_consistency=%.2f%%\n", consistency)
	fmt.Printf("keyframe_hit_rate=%.2f%%\n", straight.Manifest.KeyframeHitRate()*100)
	fmt.Printf("aligned_error_p95_ms=%.2f\n", float64(straight.AlignP95Ns)/1e6)
	fmt.Printf("segments=%d\n", len(straight.Manifest.Segments))
	fmt.Printf("segment_hash_mismatch=%d\n", mismatches)
	fmt.Printf("duplicate_dropped=%d\n", straight.DuplicatesDropped)
	fmt.Printf("loss_burst_recovered=%d\n", straight.LossBurstRecovered)
	fmt.Printf("parse_error=%d\n", straight.Errs.Count(fault.CatParse))
	fmt.Printf("timing_error=%d\n", straight.Errs.Count(fault.CatTiming))
	fmt.Printf("isolated_tracks=%d\n", len(straight.IsolatedTracks))
	fmt.Printf("rollback_items=%d\n", resumed.RollbackItems)
	fmt.Printf("throughput_pps=%.0f\n", pps)
	if *corrupt != "" {
		// Corrupt input was isolated per policy; signal with exit code 2.
		fmt.Println("exit_code=2")
		return 2
	}
	ok := consistency >= 99.0 &&
		straight.Manifest.KeyframeHitRate() >= 0.98 &&
		straight.AlignP95Ns <= 40_000_000 &&
		len(straight.Manifest.Segments) >= 12 &&
		mismatches == 0 &&
		straight.DuplicatesDropped >= 1 &&
		straight.LossBurstRecovered >= 1 &&
		!straight.PublishRejected
	if !ok {
		fmt.Println("exit_code=1")
		return 1
	}
	fmt.Println("exit_code=0")
	return 0
}

func verifyCmd(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "output directory to verify")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "error: --dir is required")
		return 1
	}
	_, lines, mismatches := pipeline.VerifyOutput(filepath.Clean(*dir))
	for _, l := range lines {
		fmt.Println(l)
	}
	fmt.Printf("segment_hash_mismatch=%d\n", mismatches)
	if mismatches > 0 {
		fmt.Println("exit_code=1")
		return 1
	}
	fmt.Println("exit_code=0")
	return 0
}

// compareManifests returns the percentage of segments with identical
// hashes between the uninterrupted and the checkpoint-recovered run.
func compareManifests(a, b *pipeline.Result) float64 {
	sa, sb := a.Manifest.Segments, b.Manifest.Segments
	if len(sa) == 0 || len(sa) != len(sb) {
		return 0
	}
	same := 0
	for i := range sa {
		if sa[i].SHA256 == sb[i].SHA256 {
			same++
		}
	}
	return float64(same) / float64(len(sa)) * 100
}

// parseBandwidth parses "2m,800k,3m" into bits-per-second steps.
func parseBandwidth(s string) ([]int64, error) {
	if s == "" {
		return nil, fmt.Errorf("empty bandwidth spec")
	}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			return nil, fmt.Errorf("empty bandwidth element in %q", s)
		}
		mult := int64(1)
		switch {
		case strings.HasSuffix(part, "m"):
			mult, part = 1_000_000, part[:len(part)-1]
		case strings.HasSuffix(part, "k"):
			mult, part = 1_000, part[:len(part)-1]
		}
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("invalid bandwidth %q", part)
		}
		out = append(out, v*mult)
	}
	return out, nil
}

// safeOutDir validates the output directory: no path traversal, and an
// existing non-empty directory is only reused when it carries our marker
// (never overwrite a non-target directory).
func safeOutDir(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("--out is required")
	}
	for _, el := range strings.Split(filepath.ToSlash(dir), "/") {
		if el == ".." {
			return "", fmt.Errorf("path traversal rejected: %q", dir)
		}
	}
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return "", err
	}
	if abs == "/" {
		return "", fmt.Errorf("refusing filesystem root as output")
	}
	info, err := os.Stat(abs)
	switch {
	case err == nil:
		if !info.IsDir() {
			return "", fmt.Errorf("output path %q is not a directory", abs)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			if _, err := os.Stat(filepath.Join(abs, markerFile)); err != nil {
				return "", fmt.Errorf("refusing to overwrite non-target directory %q", abs)
			}
			if err := pipeline.CleanDir(abs); err != nil {
				return "", err
			}
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	return abs, os.WriteFile(filepath.Join(abs, markerFile), []byte("mediaroute\n"), 0o644)
}
