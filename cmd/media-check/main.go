// Command media-check validates a synthetic broadcast delivery package:
// it unifies track timebases, inspects MP4 structure and keyframes,
// aligns timed metadata, windows RTP packets, adjudicates fault
// segments and remuxes accepted media under bounded memory.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"mediacheck/internal/app"
	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
	"mediacheck/internal/remux"
)

func main() {
	os.Exit(run())
}

func run() int {
	generate := flag.Bool("generate", false, "generate and check a synthetic package")
	profile := flag.String("profile", "broadcast", "fixture profile: normal|broadcast|truncated|corrupt|no_keyframe|drift|boundary")
	lossBurst := flag.Int("loss-burst", 12, "consecutive RTP sequence numbers dropped")
	slowSink := flag.Int("slow-sink-ms", 20, "per-chunk sink delay to induce backpressure")
	memLimit := flag.Int64("memory-limit-mib", 32, "hard memory budget in MiB")
	duration := flag.Int64("duration-ms", 2000, "synthetic media duration in ms")
	windowCap := flag.Int("window-cap", 64, "RTP reorder window capacity")
	kfGap := flag.Int64("max-keyframe-gap-ms", 2500, "maximum keyframe interval in ms")
	drift := flag.Int64("drift-ms", 0, "injected A/V clock drift in ms")
	outPath := flag.String("out", "", "optional path for the remuxed container")
	flag.Parse()

	if !*generate {
		fmt.Fprintln(os.Stderr, "media-check: only --generate mode is supported in this local build")
		return int(model.ExitInvalidArgs)
	}
	if !validProfile(*profile) {
		fmt.Fprintf(os.Stderr, "media-check: unknown profile %q\n", *profile)
		return int(model.ExitInvalidArgs)
	}

	opts := app.Options{
		Profile:    fixture.Profile(*profile),
		LossBurst:  *lossBurst,
		SlowSinkMs: *slowSink,
		MemoryMiB:  *memLimit,
		DurationMs: *duration,
		WindowCap:  *windowCap,
		MaxKfGapMs: *kfGap,
		DriftMs:    *drift,
	}
	if err := app.ValidateForCLI(opts); err != nil {
		fmt.Fprintf(os.Stderr, "media-check: %v\n", err)
		return int(model.ExitInvalidArgs)
	}

	ctx := context.Background()
	res, err := app.Run(ctx, opts)
	if err != nil {
		if res.Exit == model.ExitInvalidArgs {
			fmt.Fprintf(os.Stderr, "media-check: invalid input: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "media-check: internal failure: %v\n", err)
			if res.Exit == model.ExitOK {
				res.Exit = model.ExitInternal
			}
		}
		if len(res.Committed) > 0 {
			fmt.Fprintf(os.Stderr, "last committed boundary: %+v\n", res.Committed)
		}
		return int(res.Exit)
	}

	if *outPath != "" {
		if err := os.WriteFile(*outPath, res.Output, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "media-check: cannot write output: %v\n", err)
			return int(model.ExitInternal)
		}
	}
	fmt.Print(remux.Render(res.Summary))
	printSegments(res.Segments)
	return int(res.Exit)
}

func validProfile(p string) bool {
	switch fixture.Profile(p) {
	case fixture.ProfileNormal, fixture.ProfileBroadcast, fixture.ProfileTruncated,
		fixture.ProfileCorrupt, fixture.ProfileNoKeyframe, fixture.ProfileDrift,
		fixture.ProfileBoundary:
		return true
	}
	return false
}

func printSegments(segs []model.Segment) {
	if len(segs) == 0 {
		fmt.Println("segments none")
		return
	}
	for _, s := range segs {
		fmt.Printf("segment id=%d track=%s [%d,%d)ms recovery=%s reason=%s fault_ids=%v\n",
			s.ID, s.Track, s.StartMs, s.EndMs, s.Recovery, s.Reason, s.FaultIDs)
	}
}
