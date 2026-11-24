package audio

import (
	"errors"
	"testing"

	"review/internal/fixture"
	"review/internal/media"
	"review/internal/mkv"
)

func track(channels uint32, rate float64) mkv.Track {
	return mkv.Track{
		Number: 7, Type: fixture.TrackTypeAudio, Codec: fixture.CodecOpus,
		Channels: channels, SampleRate: rate, BitDepth: 16,
	}
}

func TestKnownLayoutsAndResampling(t *testing.T) {
	cases := []struct {
		channels uint32
		rate     float64
		ratio    float64
	}{
		{1, 48000, 1},
		{2, 44100, CanonicalSampleRate / 44100},
		{6, 24000, 2},
	}
	for _, tc := range cases {
		plan, err := BuildPlan(track(tc.channels, tc.rate), nil)
		if err != nil || !plan.Mappable() {
			t.Fatalf("plan (%d ch, %.0f): %+v err=%v", tc.channels, tc.rate, plan, err)
		}
		if plan.ResampleRatio != tc.ratio {
			t.Fatalf("ratio = %v, want %v", plan.ResampleRatio, tc.ratio)
		}
		res, err := Normalize(plan, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if res.OutputChannels != CanonicalChannels || res.OutputRate != CanonicalSampleRate {
			t.Fatalf("canonical output wrong: %+v", res)
		}
		if res.OutputFrames != int(1000*tc.ratio+0.5) {
			t.Fatalf("output frames = %d", res.OutputFrames)
		}
	}
}

func TestExplicitChannelMapAccepted(t *testing.T) {
	// 3-channel source is unknown by default but resolvable with a map.
	explicit := ChannelMap{0: ChLeft, 1: ChRight, 2: ChCenter}
	plan, err := BuildPlan(track(3, 48000), explicit)
	if err != nil || !plan.Mappable() {
		t.Fatalf("explicit map rejected: %+v err=%v", plan, err)
	}
	if plan.ChannelMap[2] != ChCenter {
		t.Fatal("explicit map not retained")
	}
}

func TestExplicitMapErrors(t *testing.T) {
	bad := []ChannelMap{
		{0: ChLeft, 2: ChRight},            // channel 1 unmapped
		{0: ChLeft, 1: ChLeft},             // duplicate target
		{0: ChLeft, 1: "X"},                // non-canonical target
	}
	for i, m := range bad {
		if _, err := BuildPlan(track(3, 48000), m); err == nil {
			t.Fatalf("bad map %d accepted", i)
		}
	}
}

func TestUnknownLayoutBlocksWithFirstDependency(t *testing.T) {
	// Remote narration omits Channels entirely.
	tr := track(0, 24000)
	plan, err := BuildPlan(tr, nil)
	var pe *media.PipelineError
	if !errors.As(err, &pe) || pe.Class != media.ClassConflict {
		t.Fatalf("want conflict pipeline error, got %v", err)
	}
	if pe.Dependency != "mkv-decode" {
		t.Fatalf("first dependency = %q, want mkv-decode", pe.Dependency)
	}
	if plan.Mappable() {
		t.Fatal("plan for unknown layout must not be mappable")
	}
	// An invalid plan must also refuse Normalize and keep the dependency.
	if _, err := Normalize(plan, 10); !errors.As(err, &pe) {
		t.Fatalf("Normalize must propagate conflict, got %v", err)
	}
}

func TestUnsupportedRateBlocks(t *testing.T) {
	_, err := BuildPlan(track(2, 11025), nil)
	var pe *media.PipelineError
	if !errors.As(err, &pe) || pe.Class != media.ClassConflict {
		t.Fatalf("unsupported rate must conflict, got %v", err)
	}
	if media.FirstBlocker(err) != "timeline-merge" {
		t.Fatalf("blocker = %q", media.FirstBlocker(err))
	}
}

func TestBlockingPropagatesToTimeline(t *testing.T) {
	// Simulate the DAG wiring: a normalize failure must be re-wrapped as a
	// timeline-merge blocker while preserving class and provenance.
	_, root := BuildPlan(track(0, 48000), nil)
	blocked := media.BlockedBy("timeline-merge", root.(*media.PipelineError))
	if blocked.Class != media.ClassConflict {
		t.Fatalf("class lost through chain: %s", blocked.Class)
	}
	if got := media.FirstBlocker(blocked); got != "mkv-decode" {
		t.Fatalf("first blocker = %q, want mkv-decode", got)
	}
}

func TestDeterministicFrameCount(t *testing.T) {
	plan, err := BuildPlan(track(2, 44100), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := Normalize(plan, 333)
	second, _ := Normalize(plan, 333)
	if first.OutputFrames != second.OutputFrames {
		t.Fatal("repeated normalization diverged")
	}
}
