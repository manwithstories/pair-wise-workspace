// Command fieldcut is the terminal-only demo entry point for the multi-camera
// post-production fragment pipeline.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/fieldcut/fieldcut/internal/audio"
)

// Acceptance thresholds from the delivery specification.
const (
	minRoundtripRate = 0.995
	minIndexHitRate  = 0.99
	hashMatchRate    = 1.0
)

// Exit codes: 0 all thresholds passed, 1 processing/verification failure,
// 2 invalid arguments.
const (
	exitOK       = 0
	exitFailure  = 1
	exitBadArgs  = 2
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitBadArgs)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemoCLI(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(exitBadArgs)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fieldcut demo --seed N --duration DUR --renditions N --inject a,b,c --repeat N [--out DIR]")
}

func runDemoCLI(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Uint64("seed", 0, "deterministic random seed")
	duration := fs.Duration("duration", 0, "synthetic program duration (e.g. 24s)")
	renditions := fs.Int("renditions", 0, "number of bitrate renditions (1..3)")
	inject := fs.String("inject", "", "comma-separated fault injections")
	repeat := fs.Int("repeat", 1, "number of full pipeline reruns for hash stability")
	out := fs.String("out", ".fieldcut-demo", "output directory (git-ignored)")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}

	if *duration <= 0 || *renditions <= 0 || *renditions > 3 || *repeat <= 0 {
		fmt.Fprintln(os.Stderr, "error: duration must be positive, renditions in 1..3, repeat >= 1")
		return exitBadArgs
	}
	injections := parseInject(*inject)
	if injections == nil {
		fmt.Fprintln(os.Stderr, "error: unknown injection; valid: trunc-au,index-drift,clock-wrap,color-conflict")
		return exitBadArgs
	}

	cfg := demoConfig{
		seed: *seed, duration: *duration, renditions: *renditions,
		inject: injections, repeat: *repeat, outDir: *out,
	}
	metrics, err := runDemo(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "processing failed: %v\n", err)
		return exitFailure
	}

	printReport(cfg, metrics)
	if !passes(metrics) {
		fmt.Println("result=FAIL")
		return exitFailure
	}
	fmt.Println("result=PASS")
	return exitOK
}

func parseInject(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	valid := map[string]bool{
		"trunc-au": true, "index-drift": true, "clock-wrap": true, "color-conflict": true,
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !valid[p] {
			return nil
		}
		out = append(out, p)
	}
	return out
}

func printReport(cfg demoConfig, m demoMetrics) {
	fmt.Println("fieldcut demo report")
	fmt.Printf("seed=%d duration=%s renditions=%d inject=%s repeat=%d\n",
		cfg.seed, cfg.duration.Round(time.Millisecond), cfg.renditions,
		strings.Join(cfg.inject, ","), cfg.repeat)
	fmt.Printf("roundtrip_rate=%.3f\n", m.roundtripRate)
	fmt.Printf("keyframe_hit=%s\n", m.keyframeHit)
	fmt.Printf("segment_hash=%s\n", m.segmentHash)
	fmt.Printf("boundary_mismatch=%d\n", m.boundaryMismatch)
	fmt.Printf("resume_hash_equal=%t\n", m.resumeHashEqual)
	fmt.Printf("audio_lufs=%.1f\n", m.audioLUFS)
}

func passes(m demoMetrics) bool {
	if m.roundtripRate+1e-9 < minRoundtripRate {
		return false
	}
	hits, total := parseFraction(m.keyframeHit)
	if total == 0 || float64(hits)/float64(total)+1e-9 < minIndexHitRate {
		return false
	}
	matched, segTotal := parseFraction(m.segmentHash)
	if segTotal == 0 || !floatEquals(float64(matched)/float64(segTotal), hashMatchRate) {
		return false
	}
	if m.boundaryMismatch != 0 || !m.resumeHashEqual {
		return false
	}
	if math.Abs(m.audioLUFS-audio.TargetLUFS) > 0.2 {
		return false
	}
	return true
}

func parseFraction(s string) (num, den int) {
	if _, err := fmt.Sscanf(s, "%d/%d", &num, &den); err != nil {
		return 0, 0
	}
	return num, den
}

func floatEquals(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
