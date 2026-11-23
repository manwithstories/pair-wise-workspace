package timeline

import (
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/sample"
)

func makeSamples(trackIdx, n int, start, stride clock.Tick) []*sample.Sample {
	out := make([]*sample.Sample, n)
	for i := range out {
		out[i] = &sample.Sample{
			TrackIndex: trackIdx, Seq: i,
			PTS: start + clock.Tick(i)*stride, Duration: stride,
			Codec: sample.CodecH264,
		}
	}
	return out
}

func TestSelectWholeSamplesOnly(t *testing.T) {
	in := makeSamples(0, 10, 0, 3000)
	// Boundary at 4500 lands between samples at 3000 and 6000.
	plan := Plan{Cuts: []Cut{{Start: 0, End: 4500}}}
	res := Select(in, plan)
	// End snaps at-or-after 4500 -> 6000, so samples with PTS < 6000 survive.
	if len(res.Samples) != 2 {
		t.Fatalf("want 2 whole samples, got %d", len(res.Samples))
	}
	for _, b := range res.Boundaries {
		if b.Requested == 4500 && b.Snapped != 6000 {
			t.Fatalf("end must snap to 6000, got %d (dev %d)", b.Snapped, b.Deviation)
		}
	}
}

func TestStartSnapsAtOrBefore(t *testing.T) {
	in := makeSamples(0, 10, 0, 3000)
	plan := Plan{Cuts: []Cut{{Start: 4500, End: 12000}}}
	res := Select(in, plan)
	// Start snaps at-or-before 4500 -> 3000.
	if res.Boundaries[0].Snapped != 3000 || res.Boundaries[0].Deviation != -1500 {
		t.Fatalf("start snap wrong: %+v", res.Boundaries[0])
	}
	if len(res.Samples) != 3 { // PTS 3000,6000,9000
		t.Fatalf("want 3 samples, got %d", len(res.Samples))
	}
}

func TestMultipleCutsNoDuplicate(t *testing.T) {
	in := makeSamples(0, 10, 0, 3000)
	plan := Plan{Cuts: []Cut{
		{Start: 0, End: 3000},
		{Start: 6000, End: 9000},
	}}
	res := Select(in, plan)
	// First cut keeps PTS 0; second keeps PTS 6000. No overlap, no duplicate.
	if len(res.Samples) != 2 {
		t.Fatalf("want 2 distinct samples, got %d", len(res.Samples))
	}
}

func TestMaxAbsDeviation(t *testing.T) {
	in := makeSamples(0, 10, 0, 3000)
	plan := Plan{Cuts: []Cut{{Start: 100, End: 8900}}}
	res := Select(in, plan)
	// Start dev = 0-100 = -100; end snaps to 9000, dev = 100.
	if got := res.MaxAbsDeviation(); got != 100 {
		t.Fatalf("max abs deviation = %d, want 100", got)
	}
}

func TestEmptyPlan(t *testing.T) {
	in := makeSamples(0, 5, 0, 3000)
	res := Select(in, Plan{})
	if len(res.Samples) != 0 || len(res.Boundaries) != 0 {
		t.Fatal("empty plan must select nothing")
	}
}
