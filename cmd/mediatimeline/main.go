// Command mediatimeline restores multi-track timing between an origin
// and edge nodes, validates the recovered frame sequence and exports a
// reviewable frame bundle. It is a terminal-only tool.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/fixture"
	"pairflow/media/internal/pipeline"
	"pairflow/media/internal/timeline"
)

const usage = `mediatimeline restores multi-track media timing and exports review frames.

Usage:
  mediatimeline demo  [flags]   generate fixtures, validate and export
  medatimeline check [flags]    run every stage without exporting frames

Flags:
  --seed              fixture seed (default 41)
  --drift-ppm         node clock drift in ppm (default 85)
  --wrap-at           32-bit RTP wrap bias (default 4294967000)
  --frame-rate        24, 25, 30 or 30000/1001 (default 30000/1001)
  --frames            number of video frames to generate (default 120)
  --out               output directory for the frame bundle
  --policy            subtitle conflict policy: last-wins|keep-both|reject
  --window            optional clip window as start:end seconds
  --jump-threshold-ms RTP forward jump threshold in ms (default 2000)
  --residual-ms       drift residual outlier threshold in ms (default 100)
  --keyframe-only     export only sync samples
  --max-frames        cap exported frames
  --continue-on-error export frames even when validation fails
  --json              emit the summary as JSON (default true for demo)

Exit codes: 0 validation passed, 1 input or validation failure, 2 bad flags.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd := args[0]
	switch cmd {
	case "demo", "check":
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mediatimeline: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	seed := fs.Int64("seed", 41, "fixture seed")
	drift := fs.Int64("drift-ppm", 85, "node clock drift in ppm")
	wrapAt := fs.Uint64("wrap-at", 4294967000, "32-bit RTP wrap bias")
	rateStr := fs.String("frame-rate", "30000/1001", "frame rate")
	frames := fs.Int("frames", 120, "video frame count")
	out := fs.String("out", "", "output directory")
	policy := fs.String("policy", "last-wins", "subtitle conflict policy")
	window := fs.String("window", "", "clip window start:end in seconds")
	jumpMS := fs.Int64("jump-threshold-ms", 2000, "RTP jump threshold")
	residualMS := fs.Int64("residual-ms", 100, "drift residual threshold")
	keyframeOnly := fs.Bool("keyframe-only", false, "export sync samples only")
	maxFrames := fs.Int("max-frames", 0, "cap exported frames")
	cont := fs.Bool("continue-on-error", false, "export despite validation failure")
	asJSON := fs.Bool("json", true, "emit JSON summary")

	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "mediatimeline: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *wrapAt > 0xFFFFFFFF {
		fmt.Fprintf(os.Stderr, "mediatimeline: --wrap-at %d exceeds 32 bits\n", *wrapAt)
		return 2
	}
	rate, err := fixture.ParseRate(*rateStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediatimeline: %v\n", err)
		return 2
	}
	pol, err := timeline.ParseConflictPolicy(*policy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediatimeline: %v\n", err)
		return 2
	}
	win, err := parseWindow(*window)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediatimeline: %v\n", err)
		return 2
	}
	render := cmd == "demo"
	if render && *out == "" {
		fmt.Fprintln(os.Stderr, "mediatimeline: demo requires --out")
		return 2
	}
	if *residualMS <= 0 || *jumpMS <= 0 {
		fmt.Fprintln(os.Stderr, "mediatimeline: thresholds must be positive")
		return 2
	}

	cfg := pipeline.Config{
		Seed: *seed, DriftPPM: *drift, WrapAt: uint32(*wrapAt),
		FrameRate: rate, FrameCount: *frames, OutDir: *out,
		Policy: pol, Window: win, ContinueOnError: *cont, Render: render,
		JumpThresholdMS: *jumpMS, ResidualThresholdMS: *residualMS,
		KeyframeOnly: *keyframeOnly, MaxFrames: *maxFrames,
	}
	sum, err := pipeline.Run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediatimeline: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sum); err != nil {
			fmt.Fprintf(os.Stderr, "mediatimeline: %v\n", err)
			return 1
		}
	} else {
		printHuman(sum)
	}
	if !sum.Passed {
		for _, f := range sum.Failures {
			fmt.Fprintf(os.Stderr, "FAIL: %s\n", f)
		}
		return 1
	}
	return 0
}

// parseWindow accepts "start:end" seconds (fractional allowed) and
// returns the zero window when empty.
func parseWindow(s string) (timeline.Window, error) {
	if s == "" {
		return timeline.Window{}, nil
	}
	var a, b float64
	if _, err := fmt.Sscanf(s, "%f:%f", &a, &b); err != nil {
		return timeline.Window{}, fmt.Errorf("bad --window %q (want start:end)", s)
	}
	if a < 0 || b <= a {
		return timeline.Window{}, fmt.Errorf("bad --window %q: end must exceed start", s)
	}
	return timeline.Window{
		Start: clockFromFloat(a),
		End:   clockFromFloat(b),
	}, nil
}

func printHuman(sum *pipeline.Summary) {
	fmt.Printf("seed=%d drift=%.1fppm rate=%s\n", sum.Seed, sum.EstimatedDriftPPM.Seconds(), sum.FrameRate)
	fmt.Printf("roundtrip=%.4f keyframe_hit=%.4f segments=%d frames=%d\n",
		sum.RoundtripRate.Seconds(), sum.KeyframeHitRate.Seconds(),
		sum.SegmentSHACount, sum.Frames)
	fmt.Printf("recovered=%d errors_with_offset=%d vfr=%d\n",
		sum.RecoveredRecords, sum.ErrorsWithOffset, sum.VFRBytes)
	fmt.Printf("rtp wraps=%d dup=%d back=%d jumps=%d anomalies=%d\n",
		sum.RTPWraps, sum.RTPDuplicates, sum.RTPBacksteps, sum.RTPJumps, sum.RTPAnomalies)
	for _, s := range sum.Stages {
		fmt.Printf("  [%-9s] ok=%v %s\n", s.Name, s.OK, s.Detail)
	}
	fmt.Printf("passed=%v\n", sum.Passed)
}

// clockFromFloat converts a flag-supplied decimal second value to an
// exact rational using integer microsecond rounding, never a float
// accumulation. Only CLI argument parsing uses this path; all pipeline
// timestamps stay exact.
func clockFromFloat(v float64) clock.Rational {
	us := int64(v*1e6 + 0.5)
	return clock.FromMicros(us)
}
