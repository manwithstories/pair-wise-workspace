package index

import (
	"testing"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
	"mediapack/internal/mp4"
)

func summary(deltas []uint32, keyframes []int, timescale uint32) *mp4.Summary {
	track := mp4.Track{ID: 1, Handler: "vide", Timescale: timescale, SampleDeltas: deltas, Keyframes: keyframes}
	return &mp4.Summary{Tracks: []mp4.Track{track}}
}

func TestBuildComputesExactStarts(t *testing.T) {
	tab, err := Build(summary([]uint32{3000, 3000, 3000}, []int{1, 3}, 90000), 1, "800k", DefaultLimits())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(tab.Keyframes) != 2 {
		t.Fatalf("keyframes=%d", len(tab.Keyframes))
	}
	if tab.Keyframes[0].Ticks.String() != "0" || tab.Keyframes[1].Ticks.String() != "1/15" {
		t.Fatalf("keyframe ticks: %s %s", tab.Keyframes[0].Ticks, tab.Keyframes[1].Ticks)
	}
	if tab.Keyframes[1].Representation != "800k" {
		t.Fatalf("representation lost")
	}
	if tab.Duration().String() != "1/10" {
		t.Fatalf("duration=%s", tab.Duration())
	}
	if tab.Keyframes[1].DeltaTicks.String() != "1/30" {
		t.Fatalf("delta=%s", tab.Keyframes[1].DeltaTicks)
	}
}

func TestBuildTreatsAbsentStssAsAllSync(t *testing.T) {
	tab, err := Build(summary([]uint32{3000, 3000}, nil, 90000), 1, "800k", DefaultLimits())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !tab.EverySample || len(tab.Keyframes) != 2 {
		t.Fatalf("every_sample=%v keyframes=%d", tab.EverySample, len(tab.Keyframes))
	}
}

func TestBuildRejectsBadSyncTables(t *testing.T) {
	if _, err := Build(summary([]uint32{3000}, []int{2}, 90000), 1, "x", DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("out of range: %v", err)
	}
	if _, err := Build(summary([]uint32{3000}, []int{0}, 90000), 1, "x", DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("zero sync sample: %v", err)
	}
	if _, err := Build(summary([]uint32{3000, 3000}, []int{1, 1}, 90000), 1, "x", DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestBuildRejectsMissingTrackAndLimits(t *testing.T) {
	if _, err := Build(summary([]uint32{3000}, []int{1}, 90000), 9, "x", DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("missing track: %v", err)
	}
	if _, err := Build(summary([]uint32{3000}, []int{1}, 0), 1, "x", DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("zero timescale: %v", err)
	}
	if _, err := Build(summary([]uint32{3000, 3000}, []int{1}, 90000), 1, "x", Limits{MaxEntries: 1}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("limit: %v", err)
	}
	if _, err := Build(summary([]uint32{3000}, []int{1}, 90000), 1, "x", Limits{MaxEntries: 0}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("zero limit should reject: %v", err)
	}
}

func TestBuildKeepsLargeDeltasExact(t *testing.T) {
	// uint32 deltas at a 1 Hz timescale are representable; the index must not
	// round them while converting to ticks.
	tab, err := Build(summary([]uint32{^uint32(0)}, []int{1}, 1), 1, "x", DefaultLimits())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if yes, _ := tab.Durations[0].ToIntExact(); uint64(yes) != uint64(^uint32(0)) {
		t.Fatalf("duration=%s", tab.Durations[0])
	}
}

func TestAtReturnsCoveringKeyframe(t *testing.T) {
	tab, err := Build(summary([]uint32{3000, 3000, 3000, 3000}, []int{1, 3}, 90000), 1, "800k", DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	kf, ok := tab.At(clock.MustNew(1, 20)) // 4500 ticks, between kf 1 and kf 3
	if !ok || kf.Sample != 1 {
		t.Fatalf("at 1/20: sample=%d ok=%v", kf.Sample, ok)
	}
	if _, ok := tab.At(clock.MustNew(-1, 90000)); ok {
		t.Fatal("negative time matched a keyframe")
	}
}

func TestCoverage(t *testing.T) {
	a := []Keyframe{{Ticks: clock.MustNew(0, 1)}, {Ticks: clock.MustNew(1, 3)}, {Ticks: clock.MustNew(2, 3)}}
	if got := Coverage(a, a); got != 100 {
		t.Fatalf("self coverage=%f", got)
	}
	b := []Keyframe{{Ticks: clock.MustNew(0, 1)}, {Ticks: clock.MustNew(1, 2)}}
	if got := Coverage(a, b); got != 50 {
		t.Fatalf("partial coverage=%f", got)
	}
	if got := Coverage(nil, nil); got != 100 {
		t.Fatalf("empty coverage=%f", got)
	}
	if got := Coverage(nil, a); got != 0 {
		t.Fatalf("no hits=%f", got)
	}
	// 1/3 and 2/6 are the same rational; coverage compares exact values.
	c := []Keyframe{{Ticks: clock.MustNew(2, 6)}}
	if got := Coverage(a, c); got != 100 {
		t.Fatalf("equivalent rationals=%f", got)
	}
}

func TestBuildNilSummary(t *testing.T) {
	if _, err := Build(nil, 1, "x", DefaultLimits()); fault.ClassOf(err) != fault.ClassInternal {
		t.Fatalf("got %v", err)
	}
}
