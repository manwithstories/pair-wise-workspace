package segments

import (
	"testing"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
	"mediapack/internal/index"
)

// table builds a synthetic index with the given frame duration and keyframe
// sample numbers.
func table(rep string, frames int, frameTicks, keyframes []int) *index.Table {
	t := &index.Table{Track: 1, Timescale: 90000}
	cur := clock.MustNew(0, 1)
	for i := 0; i < frames; i++ {
		d := clock.MustNew(int64(frameTicks[i]), 1)
		t.Starts = append(t.Starts, cur)
		t.Durations = append(t.Durations, d)
		cur, _ = cur.Add(d)
	}
	for _, n := range keyframes {
		t.Keyframes = append(t.Keyframes, index.Keyframe{Sample: n, Ticks: t.Starts[n-1],
			DeltaTicks: t.Durations[n-1], Representation: rep})
	}
	return t
}

func uniform(frames, delta int, keyframes []int) []int {
	d := make([]int, frames)
	for i := range d {
		d[i] = delta
	}
	_ = keyframes
	return d
}

func TestAlignProducesStableBoundaries(t *testing.T) {
	// 10 frames of 3000 ticks = 30000; 10000-tick segments -> 3 segments.
	// Keyframes on samples 1, 4, 7 and 10 (0/9000/18000/27000 ticks) keep
	// every boundary within one 3000-tick frame.
	primary := table("800k", 10, uniform(10, 3000, nil), []int{1, 4, 7, 10})
	secondary := table("1600k", 10, uniform(10, 3000, nil), []int{1, 4, 7, 10})
	plan, err := Align([]Representation{
		{Name: "800k", Rate: 800000, FrameDuration: clock.MustNew(3000, 1), Keyframes: primary},
		{Name: "1600k", Rate: 1600000, FrameDuration: clock.MustNew(3000, 1), Keyframes: secondary},
	}, clock.MustNew(10000, 1), DefaultLimits())
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if len(plan.Segments) != 3 {
		t.Fatalf("segments=%d want 3", len(plan.Segments))
	}
	if !plan.Segments[0].Start.IsZero() || plan.Segments[1].Start.String() != "10000" {
		t.Fatalf("boundaries wrong: %s %s", plan.Segments[0].Start, plan.Segments[1].Start)
	}
	// Boundary 10000 snaps back to the keyframe at 9000 (12000 for boundary
	// 20000), a deviation of 1000 ticks, within the 3000-tick frame.
	if !plan.Segments[1].Keyframes["800k"].Ticks.Equal(clock.MustNew(9000, 1)) {
		t.Fatalf("segment 2 primary keyframe=%s", plan.Segments[1].Keyframes["800k"].Ticks)
	}
	// Boundaries at 10000 and 20000 snap to keyframes at 9000 and 18000;
	// the largest deviation is therefore 2000 ticks, still within a frame.
	if plan.MaxDeviation.String() != "2000" {
		t.Fatalf("max deviation=%s want 2000", plan.MaxDeviation)
	}
}

func TestDeviationOverOneFrameFails(t *testing.T) {
	primary := table("800k", 10, uniform(10, 3000, nil), []int{1, 4, 7, 10})
	// The secondary keyframe closest to 10000 sits at 6000: a 4000-tick
	// deviation, greater than one 3000-tick frame.
	secondary := table("1600k", 10, uniform(10, 3000, nil), []int{1, 3, 7, 10})
	_, err := Align([]Representation{
		{Name: "800k", FrameDuration: clock.MustNew(3000, 1), Keyframes: primary},
		{Name: "1600k", FrameDuration: clock.MustNew(3000, 1), Keyframes: secondary},
	}, clock.MustNew(10000, 1), DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "boundary-misaligned" || fe.Segment != 2 {
		t.Fatalf("got %v", err)
	}
	if fe.BlockedBy != "index:1600k" {
		t.Fatalf("blocked_by=%q", fe.BlockedBy)
	}
}

func TestMissingIndexBlocksWithFirstDependency(t *testing.T) {
	primary := table("800k", 4, uniform(4, 3000, nil), []int{1})
	_, err := Align([]Representation{
		{Name: "800k", FrameDuration: clock.MustNew(3000, 1), Keyframes: primary},
		{Name: "1600k", FrameDuration: clock.MustNew(3000, 1)},
	}, clock.MustNew(10000, 1), DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Class != fault.ClassDependency || fe.BlockedBy != "index:1600k" {
		t.Fatalf("got %v", err)
	}
}

func TestShortProgramGetsSingleSegment(t *testing.T) {
	primary := table("800k", 2, uniform(2, 3000, nil), []int{1})
	plan, err := Align([]Representation{
		{Name: "800k", FrameDuration: clock.MustNew(3000, 1), Keyframes: primary},
	}, clock.MustNew(90000, 1), DefaultLimits())
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if len(plan.Segments) != 1 || plan.Segments[0].FrameCount != 2 {
		t.Fatalf("plan=%+v", plan.Segments)
	}
}

func TestAlignRejectsBadInput(t *testing.T) {
	primary := table("800k", 4, uniform(4, 3000, nil), []int{1})
	reps := []Representation{{Name: "800k", FrameDuration: clock.MustNew(3000, 1), Keyframes: primary}}
	if _, err := Align(nil, clock.MustNew(1, 1), DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("no reps: %v", err)
	}
	if _, err := Align(reps, clock.MustNew(0, 1), DefaultLimits()); fault.ClassOf(err) != fault.ClassUsage {
		t.Fatalf("zero segment: %v", err)
	}
	if _, err := Align(reps, clock.MustNew(3000, 1), Limits{MaxSegments: 1}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("segment limit: %v", err)
	}
}

func TestPlanSummary(t *testing.T) {
	primary := table("800k", 4, uniform(4, 3000, nil), []int{1, 3})
	plan, err := Align([]Representation{
		{Name: "800k", FrameDuration: clock.MustNew(3000, 1), Keyframes: primary},
	}, clock.MustNew(6000, 1), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary() == "" || len(plan.Segments) != 2 {
		t.Fatalf("summary=%q segments=%d", plan.Summary(), len(plan.Segments))
	}
}

func TestKeyframeAtBeforeFirstKeyframeFails(t *testing.T) {
	// The secondary starts 10 frames late, far beyond the one-frame tolerance.
	primary := table("800k", 20, uniform(20, 3000, nil), []int{1, 4, 7, 10, 13, 16, 19})
	secondary := table("late", 10, uniform(10, 3000, nil), []int{1})
	for i := range secondary.Starts {
		shifted, _ := secondary.Starts[i].Add(clock.MustNew(30000, 1))
		secondary.Starts[i] = shifted
	}
	for i := range secondary.Keyframes {
		shifted, _ := secondary.Keyframes[i].Ticks.Add(clock.MustNew(30000, 1))
		secondary.Keyframes[i].Ticks = shifted
	}
	_, err := Align([]Representation{
		{Name: "800k", FrameDuration: clock.MustNew(3000, 1), Keyframes: primary},
		{Name: "late", FrameDuration: clock.MustNew(3000, 1), Keyframes: secondary},
	}, clock.MustNew(9000, 1), DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "boundary-misaligned" || fe.Track != "late" {
		t.Fatalf("got %v", err)
	}
}

func TestZeroDurationPrimaryRejected(t *testing.T) {
	empty := &index.Table{Keyframes: []index.Keyframe{{Sample: 1, Ticks: clock.MustNew(0, 1)}}}
	_, err := Align([]Representation{
		{Name: "empty", FrameDuration: clock.MustNew(1, 1), Keyframes: empty},
	}, clock.MustNew(1, 1), DefaultLimits())
	if fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("got %v", err)
	}
}
