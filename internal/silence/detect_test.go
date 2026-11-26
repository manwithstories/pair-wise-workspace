package silence

import (
	"testing"

	"audiorestore/internal/model"
)

func TestDetectPerChannelRanges(t *testing.T) {
	samples := [][]int{
		{0, 0, 0, 5000, 0, 0, 0, 0},  // ch0: two silent runs
		{10, 20, 30, 40, 0, 0, 0, 0}, // ch1: one trailing run
		nil,                          // gap channel ignored
	}
	ranges := Detect("t", 48000, samples, 100, Config{Threshold: 100, MinRunFrames: 3})
	var ch0, ch1 int
	for _, r := range ranges {
		if r.Track != "t" {
			t.Fatalf("range lost track id: %+v", r)
		}
		if r.StartPTS.Cmp(model.SampleToPTS(r.StartFrame, 48000)) != 0 {
			t.Fatal("range PTS not derived from frame position")
		}
		switch r.Channel {
		case 0:
			ch0++
		case 1:
			ch1++
		default:
			t.Fatalf("unexpected channel %d", r.Channel)
		}
	}
	// ch0's first run is only 3 frames at the head (qualifies), plus a
	// 4-frame tail: both meet MinRunFrames=3.
	if ch0 != 2 {
		t.Fatalf("expected 2 silent ranges on ch0, got %d", ch0)
	}
	if ch1 != 1 {
		t.Fatalf("expected 1 silent range on ch1, got %d", ch1)
	}
	// Silent frames: ch0 head run 3 + ch0 tail 4 + ch1 tail 8 = 15.
	if total := SilentFrames(ranges); total != 15 {
		t.Fatalf("silent frame count = %d, want 15 (3+4+8)", total)
	}
}

func TestShortRunsSuppressed(t *testing.T) {
	samples := [][]int{{0, 5000, 0, 5000}}
	ranges := Detect("t", 48000, samples, 0, Config{Threshold: 100, MinRunFrames: 2})
	if len(ranges) != 0 {
		t.Fatalf("single-frame dips must be suppressed, got %d", len(ranges))
	}
}
