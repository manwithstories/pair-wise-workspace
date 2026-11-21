// Command mediad is the acceptance driver for the pairflow media pipeline.
// It is deliberately thin: every behaviour lives in internal/run and the
// other library packages; this command parses flags, maps results onto exit
// codes and prints the documented acceptance keys.
//
// Usage:
//
//	mediad -input DIR -out DIR [-queue N] [-slow-consumer 5ms]
//	       [-bitrates 360p,720p,1080p] [-random-seeks N]
//	mediad -self-check-boundaries [-input DIR]
//
// Exit codes: 0 everything verified, 1 input or processing error,
// 2 invalid parameters.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/pairflow"
	"github.com/mediamend/mediamend/internal/run"
	"github.com/mediamend/mediamend/internal/segment"
	"github.com/mediamend/mediamend/internal/verify"
)

func main() {
	os.Exit(Main(os.Args[1:]))
}

// Main is split out so tests can drive it without spawning a process.
func Main(args []string) int {
	fs := flag.NewFlagSet("mediad", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	input := fs.String("input", "", "fixture directory (required)")
	out := fs.String("out", "", "output directory (required)")
	queue := fs.Int("queue", 8, "per-stage queue capacity")
	slow := fs.Duration("slow-consumer", 0, "inject downstream latency (e.g. 5ms)")
	bitrates := fs.String("bitrates", "360p,720p,1080p", "delivery representations")
	randomSeeks := fs.Int("random-seeks", 200, "keyframe index seeks to verify")
	target := fs.Duration("segment", time.Second, "target segment duration")
	jsonOut := fs.Bool("json", false, "emit the full report as JSON")
	selfCheck := fs.Bool("self-check-boundaries", false, "run the fixture boundary self-check")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ladder, err := segment.ParseLadder(*bitrates)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediad:", err)
		return 2
	}
	if *selfCheck {
		return selfCheckBoundaries(*input, *jsonOut)
	}
	if *input == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "mediad: -input and -out are required")
		return 2
	}
	if *queue <= 0 || *randomSeeks < 0 || *target <= 0 {
		fmt.Fprintln(os.Stderr, "mediad: invalid -queue/-random-seeks/-segment")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep, err := run.Run(ctx, run.Config{
		InputDir:       *input,
		OutDir:         *out,
		QueueCap:       *queue,
		SlowConsumer:   slow.Nanoseconds(),
		Ladder:         ladder,
		TargetDuration: uint64(target.Seconds() * 90000),
		RandomSeeks:    *randomSeeks,
	})
	if rep == nil {
		fmt.Fprintln(os.Stderr, "mediad:", err)
		return 1
	}
	printReport(rep)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	}
	if rep.Fatal() {
		return 1
	}
	if err != nil {
		// Cancellation with a consistent prefix is exit 1: the delivery is
		// incomplete even though the committed segments are kept.
		fmt.Fprintln(os.Stderr, "mediad:", err)
		return 1
	}
	return 0
}

// printReport emits the acceptance keys, one per line.
func printReport(r *run.Report) {
	fmt.Printf("roundtrip_rate=%.4f\n", r.RoundTripRate)
	fmt.Printf("keyframe_hit_rate=%.3f\n", r.KeyframeHitRate)
	fmt.Printf("segment_sha256_checked=%d\n", r.SegmentSHAChecked)
	fmt.Printf("alignment_error_ticks=%d\n", r.AlignmentError)
	fmt.Printf("peak_inflight_frames=%d\n", r.PeakInflight)
	fmt.Printf("corrupt_records=%d\n", r.CorruptRecords)
	fmt.Printf("first_error_offset=%d\n", r.FirstErrorOffset)
	fmt.Printf("segments=%d\n", r.Segments)
	fmt.Printf("frames_parsed=%d\n", r.FramesParsed)
	fmt.Printf("frames_verified=%d\n", r.FramesVerified)
	fmt.Printf("duplicate_frames=%d\n", r.DuplicateFrames)
	fmt.Printf("timestamp_wraps=%d\n", r.TimestampWraps)
	fmt.Printf("missing_keyframes=%d\n", r.MissingKeyframes)
	fmt.Printf("quarantined_frames=%d\n", r.Quarantined)
	fmt.Printf("late_frames=%d\n", r.LateFrames)
	fmt.Printf("backpressure_hits=%d\n", r.BackpressureHits)
	fmt.Printf("backpressure_wait_ms=%.2f\n", float64(r.BackpressureWaitNS)/1e6)
	fmt.Printf("faults=%d\n", len(r.Faults))
	if r.Canceled {
		fmt.Println("canceled=true")
	}
	for _, f := range r.Faults {
		if f.Kind == fault.KindChecksumMismatch {
			continue // summarised by corrupt_records
		}
		fmt.Printf("fault=%s\n", f.Error())
	}
}

// selfCheckBoundaries parses each defect fixture and reports whether the
// expected defect is detected at the expected location.
func selfCheckBoundaries(input string, asJSON bool) int {
	dir := input
	if dir == "" {
		dir = os.TempDir() + "/pairflow-selfcheck"
		if _, fs := pairflow.Generate(pairflow.Options{
			Root: dir, Frames: 90, CorruptEvery: 0, CameraCount: 1,
		}); fs.Fatal() {
			fmt.Fprintln(os.Stderr, "mediad:", fs.Error())
			return 1
		}
	}
	data, err := os.ReadFile(dir + "/boundaries/samples.json")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mediad: read boundary samples:", err)
		return 1
	}
	var samples pairflow.BoundarySamples
	if err := json.Unmarshal(data, &samples); err != nil {
		fmt.Fprintln(os.Stderr, "mediad: parse boundary samples:", err)
		return 1
	}
	res := verify.SelfCheckBoundaries(verify.BoundaryPaths{
		Truncated:        samples.Truncated,
		ChecksumMismatch: samples.ChecksumMismatch,
		TimestampWrap:    samples.TimestampWrap,
		Duplicate:        samples.Duplicate,
	})
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	}
	fmt.Printf("truncated=%d checksum_mismatch=%d timestamp_wrap=%d duplicate=%d\n",
		res.Truncated, res.ChecksumMismatch, res.TimestampWrap, res.Duplicate)
	if res.OK() {
		return 0
	}
	for _, f := range res.Faults {
		fmt.Fprintf(os.Stderr, "mediad: self-check fault: %s\n", f.Error())
	}
	return 1
}

// usage documents the exit codes; kept for -h output.
func usage() string {
	return strings.Join([]string{
		"mediad -input DIR -out DIR [-queue N] [-slow-consumer 5ms]",
		"       [-bitrates 360p,720p,1080p] [-random-seeks N]",
		"mediad -self-check-boundaries [-input DIR]",
		"exit codes: 0 verified | 1 input/processing error | 2 invalid parameters",
	}, "\n")
}
