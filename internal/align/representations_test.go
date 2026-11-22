package align

import (
	"testing"

	"medialine/internal/fault"
	"medialine/internal/index"
	"medialine/internal/model"
)

func indexed(t *testing.T, id int, pts []int64, keys map[int]bool) *index.TrackIndex {
	t.Helper()
	tr := &model.Track{ID: id, Timescale: 90000}
	for i, p := range pts {
		tr.Samples = append(tr.Samples, model.Sample{Number: i + 1, PTSUS: p, DTSUS: p, DurationUS: 500, Keyframe: keys[i+1]})
	}
	idx, err := index.Build(tr)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func ladder(t *testing.T) []Representation {
	// Reps share boundaries at 0, 2s, 4s, 6s, 8s but differ inside.
	shared := []int64{0, 1_000_000, 2_000_000, 3_000_000, 4_000_000, 5_000_000, 6_000_000, 7_000_000, 8_000_000}
	keys := map[int]bool{}
	for i := range shared {
		keys[i+1] = true
	}
	// Low rep has an extra keyframe grid offset by 100ms inside segments.
	low := []int64{0, 500_000, 1_000_000, 1_500_000, 2_000_000, 2_500_000, 3_000_000, 3_500_000, 4_000_000,
		4_500_000, 5_000_000, 5_500_000, 6_000_000, 6_500_000, 7_000_000, 7_500_000, 8_000_000}
	lowKeys := map[int]bool{}
	for i := range low {
		lowKeys[i+1] = true
	}
	return []Representation{
		{ID: "v1080", Bitrate: 6_000_000, TrackID: 1, Index: indexed(t, 1, shared, keys)},
		{ID: "v720", Bitrate: 3_000_000, TrackID: 2, Index: indexed(t, 2, low, lowKeys)},
	}
}

func TestAlignSwitchableBoundaries(t *testing.T) {
	reps := ladder(t)
	got, err := Align(reps, 8_000_000, Config{SegmentUS: 2_000_000, SnapBack: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Boundaries) != 5 {
		t.Fatalf("boundaries=%d: %+v", len(got.Boundaries), got.Boundaries)
	}
	want := []int64{0, 2_000_000, 4_000_000, 6_000_000, 8_000_000}
	for i, w := range want {
		if got.Boundaries[i].EffectiveUS != w {
			t.Errorf("boundary %d=%d want %d", i, got.Boundaries[i].EffectiveUS, w)
		}
		if !got.Boundaries[i].Switchable {
			t.Errorf("boundary %d should be switchable: %+v", i, got.Boundaries[i])
		}
	}
	if len(got.Gaps) != 0 {
		t.Fatalf("gaps=%v", got.Gaps)
	}
	if got.MaxGap() != 0 {
		t.Fatalf("max gap=%d", got.MaxGap())
	}
}

func TestAlignSnapsToEarlierKeyframeAndReportsOffset(t *testing.T) {
	// Only keyframes at 0 and 1.7s; request boundaries at 0 and 2s.
	reps := []Representation{
		{ID: "a", TrackID: 1, Index: indexed(t, 1, []int64{0, 1_700_000, 3_000_000}, map[int]bool{1: true, 2: true, 3: true})},
		{ID: "b", TrackID: 2, Index: indexed(t, 2, []int64{0, 1_600_000, 3_000_000}, map[int]bool{1: true, 2: true, 3: true})},
	}
	got, err := Align(reps, 3_000_000, Config{SegmentUS: 2_000_000, SnapBack: true})
	if err != nil {
		t.Fatal(err)
	}
	// 2s snaps back to 1.6s (the earliest of the two snapped values).
	if got.Boundaries[1].EffectiveUS != 1_600_000 {
		t.Fatalf("boundary=%+v", got.Boundaries[1])
	}
	// The requested→effective delta is reported to callers.
	if d := got.Boundaries[1].RequestedUS - got.Boundaries[1].EffectiveUS; d != 400_000 {
		t.Fatalf("delta=%d", d)
	}
}

func TestAlignMissingKeyframeMarksGapButKeepsOutput(t *testing.T) {
	// Rep b has no keyframe at 2s, only at 0 and 4s.
	reps := []Representation{
		{ID: "a", TrackID: 1, Index: indexed(t, 1, []int64{0, 2_000_000, 4_000_000}, map[int]bool{1: true, 2: true, 3: true})},
		{ID: "b", TrackID: 2, Index: indexed(t, 2, []int64{0, 4_000_000}, map[int]bool{1: true, 2: true})},
	}
	got, err := Align(reps, 4_000_000, Config{SegmentUS: 2_000_000, SnapBack: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Gaps["b"]) == 0 {
		t.Fatal("expected gap for b")
	}
	if len(got.Boundaries) != 3 {
		t.Fatalf("must keep usable boundaries: %+v", got.Boundaries)
	}
	if got.Boundaries[1].Switchable {
		t.Fatal("boundary at 2s must be non-switchable")
	}
	if got.MaxGap() != 2_000_000 {
		t.Fatalf("maxgap=%d", got.MaxGap())
	}
}

func TestAlignRequireAllReturnsFault(t *testing.T) {
	reps := []Representation{
		{ID: "a", TrackID: 1, Index: indexed(t, 1, []int64{0, 2_000_000, 4_000_000}, map[int]bool{1: true, 2: true, 3: true})},
		{ID: "b", TrackID: 2, Index: indexed(t, 2, []int64{0, 4_000_000}, map[int]bool{1: true, 2: true})},
	}
	_, err := Align(reps, 4_000_000, Config{SegmentUS: 2_000_000, RequireAll: true})
	fe, ok := fault.As(err)
	if !ok || fe.Stage != fault.StageAlign {
		t.Fatalf("expected align fault, got %v", err)
	}
}

func TestAlignRejectsBadConfig(t *testing.T) {
	if _, err := Align(nil, 1000, Config{SegmentUS: 1000}); err == nil {
		t.Fatal("expected error for no reps")
	}
	reps := ladder(t)
	if _, err := Align(reps, 1000, Config{}); err == nil {
		t.Fatal("expected error for zero segment")
	}
	if _, err := Align(reps, 0, Config{SegmentUS: 1000}); err == nil {
		t.Fatal("expected error for zero span")
	}
	if _, err := Align([]Representation{{ID: "x"}}, 1000, Config{SegmentUS: 1000}); err == nil {
		t.Fatal("expected error for missing index")
	}
}

func TestBoundaryDelta(t *testing.T) {
	reps := ladder(t)
	got, err := Align(reps, 8_000_000, Config{SegmentUS: 2_000_000, SnapBack: true})
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]*index.TrackIndex{"v1080": reps[0].Index, "v720": reps[1].Index}
	if d := got.BoundaryDelta([]string{"v1080", "v720"}, idx); d != 0 {
		t.Fatalf("delta=%d", d)
	}
}
