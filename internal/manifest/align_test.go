package manifest

import (
	"strings"
	"testing"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
)

func kfEvery(stepMs, count int64, offsetMs int64) ([]int64, []clock.Time) {
	var nums []int64
	var ts []clock.Time
	for i := int64(0); i < count; i++ {
		nums = append(nums, i+1)
		ts = append(ts, clock.Must(offsetMs+i*stepMs, 1000))
	}
	return nums, ts
}

func TestAlignThreeRepresentations(t *testing.T) {
	var reps []Representation
	for i, br := range []int{800, 1800, 3500} {
		nums, ts := kfEvery(2000, 31, 0) // keyframes exactly on 2 s grid
		reps = append(reps, Representation{
			Name: string(rune('a' + i)), BitrateKbps: br,
			KeyframeNums: nums, Keyframes: ts, Duration: clock.Must(60, 1),
		})
	}
	m, err := Align(reps, clock.Must(2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if m.Representations != 3 {
		t.Fatalf("representations = %d", m.Representations)
	}
	if len(m.Segments) != 30 {
		t.Fatalf("segments = %d, want 30", len(m.Segments))
	}
	if m.MaxBoundarySkewMs != 0 {
		t.Fatalf("max skew = %d ms", m.MaxBoundarySkewMs)
	}
	if len(m.Faults) != 0 {
		t.Fatalf("faults = %v", m.Faults)
	}
	if len(m.Segments[0].Boundaries) != 3 || m.Segments[0].Boundaries[0].KeyframeSample != 1 {
		t.Fatalf("boundaries = %+v", m.Segments[0].Boundaries)
	}
}

func TestAlignSparseKeyframesFallBack(t *testing.T) {
	// Keyframes every 3 s on a 2 s segment grid: odd boundaries fall back
	// up to 1 s and the offset must be recorded.
	nums, ts := kfEvery(3000, 11, 0)
	reps := []Representation{
		{Name: "v", KeyframeNums: nums, Keyframes: ts, Duration: clock.Must(30, 1)},
	}
	m, err := Align(reps, clock.Must(2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxBoundarySkewMs != 2000 {
		t.Fatalf("max skew = %d, want 2000", m.MaxBoundarySkewMs)
	}
	// Boundary at 2 s must snap to the keyframe at 0 s (sample 1).
	b := m.Segments[1].Boundaries[0]
	if b.Actual.Millis() != 0 || b.KeyframeSample != 1 || b.SkewMs != 2000 {
		t.Fatalf("boundary = %+v", b)
	}
}

func TestAlignMissingKeyframeReported(t *testing.T) {
	// First keyframe at 10 s: the boundary at 0 ms has nothing to fall
	// back to.
	nums := []int64{1, 2, 3}
	ts := []clock.Time{clock.Must(10, 1), clock.Must(12, 1), clock.Must(14, 1)}
	reps := []Representation{
		{Name: "v", KeyframeNums: nums, Keyframes: ts, Duration: clock.Must(12, 1)},
	}
	m, err := Align(reps, clock.Must(2, 1))
	if err != nil {
		t.Fatal(err)
	}
	var fl *fault.Fault
	for _, f := range m.Faults {
		if f.Kind == fault.KindMissingKeyframe {
			fl = f
			break
		}
	}
	if fl == nil {
		t.Fatal("no missing-keyframe fault")
	}
	if fl.FirstBlocked() != "segment" {
		t.Fatalf("chain = %v", fl.Chain)
	}
	if !strings.Contains(fl.Message, "common boundary 0 ms") {
		t.Fatalf("message = %q", fl.Message)
	}
}

func TestAlignSkewWithinTolerance(t *testing.T) {
	// Keyframes offset 30 ms behind the grid: skew stays under 40 ms.
	nums, ts := kfEvery(2000, 11, 1970)
	var reps []Representation
	for i, br := range []int{800, 1800, 3500} {
		reps = append(reps, Representation{
			Name: string(rune('a' + i)), BitrateKbps: br,
			KeyframeNums: nums, Keyframes: ts, Duration: clock.Must(20, 1),
		})
	}
	m, err := Align(reps, clock.Must(2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxBoundarySkewMs > 40 {
		t.Fatalf("max skew = %d ms, want <= 40", m.MaxBoundarySkewMs)
	}
}
