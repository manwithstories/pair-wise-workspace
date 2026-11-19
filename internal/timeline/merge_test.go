package timeline

import (
	"testing"

	"archivetl/internal/clock"
)

func tick(ms int64) clock.Tick { return clock.MillisToTick(ms) }

func sample() []Item {
	return []Item{
		{Track: "B", Sequence: 1, Priority: 5, Start: tick(100), End: tick(500), Text: "b1"},
		{Track: "A", Sequence: 1, Priority: 1, Start: tick(200), End: tick(600), Text: "a1"},
		{Track: "B", Sequence: 2, Priority: 5, Start: tick(500), End: tick(900), Text: "b2"},
		{Track: "A", Sequence: 2, Priority: 1, Start: tick(100), End: tick(150), Text: "a0"},
	}
}

func TestStablePriorityOrder(t *testing.T) {
	lanes, sum := Merge(sample(), tick(10_000))
	flat := Flatten(lanes)
	if len(flat) != 4 || sum.Accepted != 4 || sum.RejectedCount != 0 {
		t.Fatalf("acceptance wrong: %d events, %+v", len(flat), sum)
	}
	// Same start 100ms: priority 1 (track A) must precede priority 5.
	if flat[0].Text != "a0" || flat[1].Text != "b1" {
		t.Fatalf("priority order wrong: %s before %s", flat[0].Text, flat[1].Text)
	}
	// Overlapping cues must not share a lane; back-to-back cues reuse one.
	if flat[0].Lane == flat[1].Lane {
		t.Fatal("overlapping events packed into the same lane")
	}
	b1 := flat[1]
	b2 := flat[3]
	if b1.Lane != b2.Lane {
		t.Fatalf("back-to-back cues should reuse lane: %d vs %d", b1.Lane, b2.Lane)
	}
}

func TestDeterminism(t *testing.T) {
	first, _ := Merge(sample(), tick(10_000))
	for i := 0; i < 5; i++ {
		again, _ := Merge(sample(), tick(10_000))
		if len(again) != len(first) {
			t.Fatal("lane count varies between runs")
		}
		for k := range first {
			if first[k].Name != again[k].Name ||
				len(first[k].Events) != len(again[k].Events) {
				t.Fatal("non-deterministic merge output")
			}
		}
	}
}

func TestZeroDurationAndOverflow(t *testing.T) {
	items := []Item{
		{Track: "X", Sequence: 1, Offset: 10, Start: tick(100), End: tick(100), Text: "zero"},
		{Track: "X", Sequence: 2, Offset: 20, Start: tick(200), End: tick(9_000), Text: "long"},
		{Track: "X", Sequence: 3, Offset: 30, Start: tick(300), End: tick(400), Text: "ok"},
	}
	lanes, sum := Merge(items, tick(1_000))
	flat := Flatten(lanes)
	if len(flat) != 2 { // zero-duration retained, overflow excluded
		t.Fatalf("expected 2 retained events, got %d", len(flat))
	}
	if sum.ZeroDuration != 1 || sum.Overflow != 1 || sum.RejectedCount != 1 {
		t.Fatalf("summary wrong: %+v", sum)
	}
	if len(sum.Failures) != 2 {
		t.Fatalf("failures=%d want 2", len(sum.Failures))
	}
	for _, f := range sum.Failures {
		if f.Offset != 10 && f.Offset != 20 {
			t.Fatalf("failure lost its source offset: %+v", f)
		}
	}
	if sum.Failures[0].Kind != FailZeroDuration {
		t.Fatalf("first failure kind=%s", sum.Failures[0].Kind)
	}
}

func TestEmptyInput(t *testing.T) {
	lanes, sum := Merge(nil, tick(1_000))
	if lanes != nil || sum.Accepted != 0 || len(sum.Failures) != 0 {
		t.Fatal("empty merge must yield no lanes and no failures")
	}
}

func TestTieBreaksOnTrackAndSequence(t *testing.T) {
	items := []Item{
		{Track: "Z", Sequence: 2, Priority: 1, Start: tick(100), End: tick(200)},
		{Track: "Z", Sequence: 1, Priority: 1, Start: tick(100), End: tick(200)},
		{Track: "A", Sequence: 9, Priority: 1, Start: tick(100), End: tick(200)},
	}
	flat := Flatten(MustLanes(items))
	if flat[0].Track != "A" || flat[1].Sequence != 1 || flat[2].Sequence != 2 {
		t.Fatalf("tie break order wrong: %+v %+v %+v", flat[0], flat[1], flat[2])
	}
}

func MustLanes(items []Item) []Lane {
	lanes, _ := Merge(items, 0)
	return lanes
}
