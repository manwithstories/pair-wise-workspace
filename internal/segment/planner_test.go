package segment

import (
	"testing"

	"avcore/internal/adapter"
	"avcore/internal/caption"
	"avcore/internal/clock"
)

func kfs(ticks ...int64) []adapter.Keyframe {
	out := make([]adapter.Keyframe, len(ticks))
	for i, t := range ticks {
		out[i] = adapter.Keyframe{TimeTicks: t}
	}
	return out
}

func TestBuildSnapsToKeyframes(t *testing.T) {
	sec := clock.TicksPerSecond
	// Keyframes every second; 1 s target should land exactly on them.
	kf := kfs(0, sec, 2*sec, 3*sec)
	plan, err := Build(kf, 4*sec, Options{TargetTicks: sec})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Cuts) != 4 {
		t.Fatalf("cuts = %d, want 4", len(plan.Cuts))
	}
	for i, c := range plan.Cuts {
		if c.StartTicks != int64(i)*sec || c.EndTicks != int64(i+1)*sec {
			t.Errorf("cut %d = %+v, want exact second boundary", i, c)
		}
		if !c.OnKeyframe {
			t.Errorf("cut %d not marked on keyframe", i)
		}
	}
	if plan.Cuts[3].EndTicks != 4*sec {
		t.Error("final cut must end exactly at duration")
	}
}

func TestBuildNearestSnapAndMonotonic(t *testing.T) {
	sec := clock.TicksPerSecond
	// Keyframes slightly offset from the 1 s grid; planner snaps to nearest.
	kf := kfs(0, sec+9000, 2*sec-9000, 3*sec+4500)
	plan, err := Build(kf, 4*sec, Options{TargetTicks: sec})
	if err != nil {
		t.Fatal(err)
	}
	var prev int64
	for i, c := range plan.Cuts {
		if c.StartTicks < prev {
			t.Fatalf("non-monotonic cut at %d", i)
		}
		if c.EndTicks <= c.StartTicks {
			t.Fatalf("zero-length cut at %d", i)
		}
		prev = c.StartTicks
	}
	// First boundary is always zero.
	if plan.Cuts[0].StartTicks != 0 {
		t.Error("first cut must start at zero")
	}
}

func TestBuildSharedPlanZeroAlignment(t *testing.T) {
	sec := clock.TicksPerSecond
	kf := kfs(0, sec, 2*sec, 3*sec)
	// Two independent planners over the same keyframes must yield identical
	// boundaries, proving renditions never diverge beyond zero ticks.
	p1, _ := Build(kf, 4*sec, Options{TargetTicks: sec})
	p2, _ := Build(kfs(0, sec, 2*sec, 3*sec), 4*sec, Options{TargetTicks: sec})
	if len(p1.Cuts) != len(p2.Cuts) {
		t.Fatal("cut count mismatch across renditions")
	}
	for i := range p1.Cuts {
		if p1.Cuts[i].StartTicks != p2.Cuts[i].StartTicks {
			t.Errorf("rendition alignment drift at cut %d: %d vs %d",
				i, p1.Cuts[i].StartTicks, p2.Cuts[i].StartTicks)
		}
	}
	if p1.MaxAlignmentMillis() != 0 {
		t.Error("shared plan must report zero alignment error")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, err := Build(kfs(0), 0, Options{TargetTicks: 1}); err == nil {
		t.Error("zero duration must be rejected")
	}
	if _, err := Build(kfs(0), clock.TicksPerSecond, Options{TargetTicks: 0}); err == nil {
		t.Error("zero target must be rejected")
	}
}

func TestBuildDedupKeyframes(t *testing.T) {
	sec := clock.TicksPerSecond
	kf := kfs(0, 0, sec, sec, 2*sec)
	plan, err := Build(kf, 3*sec, Options{TargetTicks: sec})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Cuts) != 3 {
		t.Errorf("duplicate keyframes created %d cuts, want 3", len(plan.Cuts))
	}
}

func TestSliceCaptions(t *testing.T) {
	sec := clock.TicksPerSecond
	plan, _ := Build(kfs(0, sec, 2*sec), 3*sec, Options{TargetTicks: sec})
	cues := []caption.Cue{
		{Index: 1, StartTicks: 100, EndTicks: 500, Text: "a"},
		{Index: 2, StartTicks: sec + 100, EndTicks: sec + 500, Text: "b"},
		{Index: 3, StartTicks: sec + 200, EndTicks: sec + 600, Text: "c"},
		{Index: 4, StartTicks: 3*sec + 10, EndTicks: 3*sec + 90, Text: "past-end"},
	}
	slices := SliceCaptions(plan, cues)
	if len(slices) != 3 {
		t.Fatalf("slices = %d, want 3", len(slices))
	}
	if len(slices[0].Cues) != 1 || slices[0].Cues[0].Text != "a" {
		t.Errorf("cut 0 cues wrong: %+v", slices[0].Cues)
	}
	if len(slices[1].Cues) != 2 {
		t.Errorf("cut 1 should hold two cues, got %d", len(slices[1].Cues))
	}
	if len(slices[2].Cues) != 0 {
		t.Errorf("cut 2 should be empty, got %+v", slices[2].Cues)
	}
	// Out-of-range cue must not appear anywhere.
	total := 0
	for _, s := range slices {
		total += len(s.Cues)
	}
	if total != 3 {
		t.Errorf("past-end cue leaked: total %d, want 3", total)
	}
}

func TestCutIndexAtBinarySearch(t *testing.T) {
	sec := clock.TicksPerSecond
	plan, _ := Build(kfs(0, sec, 2*sec), 3*sec, Options{TargetTicks: sec})
	if idx := cutIndexAt(plan.Cuts, 0); idx != 0 {
		t.Errorf("tick 0 -> cut %d, want 0", idx)
	}
	if idx := cutIndexAt(plan.Cuts, sec); idx != 1 {
		t.Errorf("tick at boundary -> cut %d, want 1", idx)
	}
	if idx := cutIndexAt(plan.Cuts, -1); idx != -1 {
		t.Errorf("negative tick -> %d, want -1", idx)
	}
}
