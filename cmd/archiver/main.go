// Command archiver is the terminal entry point for the archive
// restoration pipeline.
//
//	archiver demo   --case clean|damaged|recovery --seed N [--inject a,b]
//	                [--interrupt-after N] [--out DIR]
//	archiver resume --out DIR
//
// Exit codes: 0 assertions passed, 1 invariant/hash mismatch,
// 2 parameter error or unrecoverable input, 75 checkpoint saved.
package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"

	"archiver/internal/app"
)

const (
	exitOK         = 0
	exitInvariant  = 1
	exitUsage      = 2
	exitCheckpoint = 75
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return demo(args[1:])
	case "resume":
		return resume(args[1:])
	default:
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  archiver demo   --case clean|damaged|recovery --seed N [--inject a,b] [--interrupt-after N] [--out DIR]
  archiver resume --out DIR`)
}

func demo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	caze := fs.String("case", "", "clean|damaged|recovery")
	seed := fs.Uint64("seed", 1, "deterministic seed")
	inject := fs.String("inject", "", "comma separated fault injections")
	interruptAfter := fs.Int("interrupt-after", 0, "save checkpoint after N segments and exit 75")
	out := fs.String("out", "", "output directory (default: temp dir)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch *caze {
	case "clean", "damaged", "recovery":
	default:
		fmt.Fprintln(os.Stderr, "error: --case must be clean, damaged or recovery")
		return exitUsage
	}
	dir := *out
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "archiver-demo-"); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitUsage
		}
	}
	cfg := app.Config{
		Case: *caze, Seed: *seed, Out: dir, InterruptAfter: *interruptAfter,
	}
	if *inject != "" {
		cfg.Inject = strings.Split(*inject, ",")
	}
	m, err := app.Run(cfg)
	if code, ok := handleErr(err); !ok {
		return code
	}
	if m.Interrupted {
		fmt.Printf("checkpoint_saved=true segments_committed=%d out=%s\n", m.Segments, dir)
		fmt.Println("awaiting_resume=true")
		return exitCheckpoint
	}
	printMetrics(m)
	if *caze == "damaged" {
		return assertDamaged(m)
	}
	return assertClean(m)
}

func resume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	out := fs.String("out", "", "output directory holding checkpoint.json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "error: --out is required")
		return exitUsage
	}
	m, err := app.Run(app.Config{Out: *out, Resume: true})
	if code, ok := handleErr(err); !ok {
		return code
	}
	printMetrics(m)
	fmt.Printf("reused_segments=%d\n", m.ReusedSegments)
	fmt.Printf("fresh_hash_match=%.2f%%\n", m.FreshHashMatch)
	if m.FreshHashMatch != 100 || m.SegmentHashMatch != 100 || m.ReusedSegments == 0 {
		fmt.Fprintln(os.Stderr, "FAIL: resume invariants violated")
		return exitInvariant
	}
	fmt.Println("PASS")
	return exitOK
}

func handleErr(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var ie *app.InputError
	if errors.As(err, &ie) {
		fmt.Fprintln(os.Stderr, "error:", ie.Reason)
		return exitUsage, false
	}
	fmt.Fprintln(os.Stderr, "internal error:", err)
	return exitInvariant, false
}

func printMetrics(m *app.Metrics) {
	fmt.Printf("parse_roundtrip=%.2f%%\n", m.RoundTrip)
	if m.FirstErrorOffset >= 0 {
		fmt.Printf("first_error_offset=%d\n", m.FirstErrorOffset)
	}
	fmt.Printf("lost=%d duplicates=%d reordered=%d\n", m.Lost, m.Duplicates, m.Reordered)
	fmt.Printf("replays=%d quarantined=%d\n", m.Replays, m.Quarantined)
	fmt.Printf("keyframe_index_hit=%.2f%%\n", m.KeyframeHit)
	fmt.Printf("segments=%d\n", m.Segments)
	fmt.Printf("segment_hash_match=%.2f%%\n", m.SegmentHashMatch)
	fmt.Printf("sample_rate=%d\n", m.SampleRate)
	fmt.Printf("layout=%s\n", m.Layout)
	fmt.Printf("integrated_lufs=%.2f\n", m.IntegratedLUFS)
	fmt.Printf("true_peak_dbtp=%.2f\n", m.TruePeakDBTP)
}

func assertClean(m *app.Metrics) int {
	ok := m.RoundTrip == 100 &&
		m.KeyframeHit == 100 &&
		m.Segments >= 5 &&
		m.SegmentHashMatch == 100 &&
		m.SampleRate == 48000 &&
		m.Layout == "stereo" &&
		math.Abs(m.IntegratedLUFS-(-18.0)) <= 0.5
	if !ok {
		fmt.Fprintln(os.Stderr, "FAIL: acceptance invariants violated")
		return exitInvariant
	}
	fmt.Println("PASS")
	return exitOK
}

func assertDamaged(m *app.Metrics) int {
	ok := m.FirstErrorOffset >= 0 &&
		m.Duplicates > 0 &&
		m.Replays > 0 &&
		m.Quarantined > 0 &&
		m.SegmentHashMatch == 100
	if !ok {
		fmt.Fprintln(os.Stderr, "FAIL: damaged-path invariants violated")
		return exitInvariant
	}
	fmt.Println("PASS")
	return exitOK
}
