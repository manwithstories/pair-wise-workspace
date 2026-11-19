// Command mediapack prepares MP4/MOV recordings for low-latency interactive
// replay: it probes containers, cuts keyframe-aligned segments, aligns
// bitrate ladders on a unified timeline, checkpoints progress and emits a
// verified segment manifest. Terminal-only; no network or transcoding.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"mediapack/internal/clock"
	"mediapack/internal/demux"
	"mediapack/internal/fault"
	"mediapack/internal/fixture"
	"mediapack/internal/index"
	"mediapack/internal/metrics"
	"mediapack/internal/pipeline"
)

// Exit codes: 0 ok, 2 input/parameter error, 3 unrecoverable media error,
// 4 checkpoint/input mismatch.
const (
	exitOK = iota
	exitMediaOK
	exitParam
	exitMedia
	exitCheckpoint
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitParam
	}
	var code int
	switch args[0] {
	case "fixture":
		code = cmdFixture(args[1:])
	case "inspect":
		code = cmdInspect(args[1:])
	case "build":
		code = cmdBuild(args[1:], false)
	case "resume":
		code = cmdBuild(args[1:], true)
	case "verify":
		code = cmdVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return exitParam
	}
	return code
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: mediapack <command> [flags]
  fixture  --dir DIR                          generate sample material
  inspect  --input FILE                       probe tracks, timeline and index
  build    --input FILE --out DIR [flags]     cut segments and write manifest
  resume   (same flags as build, resumes from last checkpoint)
  verify   --input FILE --manifest FILE       re-check manifest against input
build flags: --segment 2s --checkpoint-samples 128 --target-bitrates 450000,900000
             --tolerance 80ms --policy strict|skip --resume
exit codes: 0 ok, 2 input/parameter, 3 unrecoverable media, 4 checkpoint mismatch
`)
}

func cmdFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	dir := fs.String("dir", "", "output directory")
	if fs.Parse(args) != nil || *dir == "" {
		return exitParam
	}
	if err := fixture.Generate(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		return exitParam
	}
	fmt.Printf("fixtures written to %s: valid.mov truncated.mov noindex.mov corrupt.mov\n", *dir)
	return exitOK
}

func cmdInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	input := fs.String("input", "", "input MP4/MOV file")
	if fs.Parse(args) != nil || *input == "" {
		return exitParam
	}
	c, err := demux.Open(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "inspect:", err)
		return exitParam
	}
	fmt.Printf("input: %s (%d bytes)\n", c.Path, c.Size)
	fmt.Printf("fingerprint: %s\n", c.Fingerprint)
	fmt.Printf("duration_ms: %.0f\n", clock.Millis(c.DurationTicks))
	for _, t := range c.Tracks {
		kfs := 0
		for _, s := range t.Samples {
			if s.Keyframe {
				kfs++
			}
		}
		fmt.Printf("track %d: %s codec=%s timescale=%d samples=%d keyframes=%d duration_ms=%.0f index_complete=%v\n",
			t.ID, t.Kind, t.Codec, t.Timescale, len(t.Samples), kfs,
			clock.Millis(clock.Ticks(t.Duration(), t.Timescale)), t.IndexComplete)
	}
	for _, iv := range c.Missing {
		fmt.Printf("missing_interval: %.0fms..%.0fms %s\n",
			clock.Millis(iv.StartTicks), clock.Millis(iv.EndTicks), iv.Reason)
	}
	for _, iv := range c.Untrusted {
		fmt.Printf("untrusted_interval: %.0fms..%.0fms %s\n",
			clock.Millis(iv.StartTicks), clock.Millis(iv.EndTicks), iv.Reason)
	}
	printFaults(c.Faults)
	if c.Faults.MaxSeverity() >= fault.Fatal {
		return exitMedia
	}
	return exitOK
}

func cmdBuild(args []string, forceResume bool) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	input := fs.String("input", "", "input MP4/MOV file")
	out := fs.String("out", "", "output directory")
	segment := fs.Duration("segment", 2*time.Second, "target segment duration")
	tolerance := fs.Duration("tolerance", 80*time.Millisecond, "alignment tolerance")
	ckptSamples := fs.Uint64("checkpoint-samples", 128, "samples between checkpoints")
	targets := fs.String("target-bitrates", "", "comma-separated target bitrates")
	policy := fs.String("policy", "strict", "fault policy: strict|skip")
	resume := fs.Bool("resume", false, "resume from last checkpoint")
	if fs.Parse(args) != nil || *input == "" || *out == "" {
		return exitParam
	}
	bitrateList, err := parseBitrates(*targets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		return exitParam
	}
	pol := fault.PolicyStrict
	if *policy == "skip" {
		pol = fault.PolicySkip
	}
	res, err := pipeline.Build(pipeline.BuildOptions{
		Input: *input, Out: *out,
		SegmentMs:         segment.Milliseconds(),
		ToleranceMs:       tolerance.Milliseconds(),
		CheckpointSamples: *ckptSamples,
		TargetBitrates:    bitrateList,
		Policy:            pol,
		Resume:            *resume || forceResume,
	})
	if err != nil {
		if errors.Is(err, index.ErrMismatch) {
			fmt.Fprintln(os.Stderr, "checkpoint mismatch:", err)
			return exitCheckpoint
		}
		fmt.Fprintln(os.Stderr, "build:", err)
		return exitParam
	}
	printFaults(res.Faults)
	if res.Blocking {
		fmt.Fprintln(os.Stderr, "unrecoverable media errors; aborting")
		return exitMedia
	}
	metrics.Render(os.Stdout, res.Manifest)
	return exitOK
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	input := fs.String("input", "", "input MP4/MOV file")
	manifestPath := fs.String("manifest", "", "manifest.json to verify")
	if fs.Parse(args) != nil || *input == "" || *manifestPath == "" {
		return exitParam
	}
	m, faults, err := pipeline.Verify(*input, *manifestPath)
	printFaults(faults)
	if err != nil {
		if pipeline.IsManifestMismatch(err) {
			fmt.Fprintln(os.Stderr, "verify failed:", err)
			return exitParam
		}
		fmt.Fprintln(os.Stderr, "verify:", err)
		return exitParam
	}
	if faults != nil && faults.MaxSeverity() >= fault.Fatal {
		return exitMedia
	}
	metrics.Render(os.Stdout, m)
	fmt.Println("verify: ok")
	return exitOK
}

func parseBitrates(s string) ([]uint64, error) {
	if s == "" {
		return nil, nil
	}
	var out []uint64
	for _, part := range strings.Split(s, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid bitrate %q: %w", part, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func printFaults(fl *fault.List) {
	if fl == nil {
		return
	}
	for _, f := range fl.Faults {
		fmt.Fprintf(os.Stderr, "fault: %s\n", f)
	}
}
