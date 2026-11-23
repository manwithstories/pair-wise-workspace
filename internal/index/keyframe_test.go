package index

import (
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/model"
)

func buildTimeline(t *testing.T, starts []int64, keyframes map[int]bool, step int64) *clock.Timeline {
	t.Helper()
	tr := model.Track{ID: "cam", Kind: model.KindVideo, Timescale: 1000, ClockBits: 32, NominalTickDelta: uint64(step), OriginNS: 2_000_000_000}
	frames := make([]model.Frame, len(starts))
	for i := range starts {
		frames[i] = model.Frame{TrackID: "cam", Seq: uint64(i), RawTS: uint64(starts[i]), Keyframe: keyframes[i], Payload: []byte{1}}
	}
	tl, err := clock.Build([]model.Track{tr}, map[string][]model.Frame{"cam": frames}, clock.Options{NoDriftCorrection: true})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestSparseKeyframesWithNonZeroStart(t *testing.T) {
	starts := []int64{500, 600, 700, 800, 900, 1000}
	kf := map[int]bool{0: true, 3: true}
	tl := buildTimeline(t, starts, kf, 100)
	idx, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	ti := idx.Tracks["cam"]
	if len(ti.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(ti.Entries))
	}
	// Target between keyframes returns the earlier one.
	res, err := idx.QueryTrack("cam", 2_350_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Hit || res.Entry.Seq != 3 || res.DeltaNS != 50_000_000 {
		t.Fatalf("query 2.35s = %+v", res)
	}
	// Exact hit.
	res, _ = idx.QueryTrack("cam", 2_300_000_000)
	if !res.Hit || res.DeltaNS != 0 || res.Entry.Seq != 3 {
		t.Fatalf("exact query = %+v", res)
	}
	// Before the first keyframe: clean miss, no error.
	res, err = idx.QueryTrack("cam", 1_999_999_999)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hit {
		t.Fatalf("expected miss before first keyframe, got %+v", res)
	}
	// After the last keyframe keeps returning the last one.
	res, _ = idx.QueryTrack("cam", 12_000_000_000)
	if !res.Hit || res.Entry.Seq != 3 {
		t.Fatalf("after-last query = %+v", res)
	}
	// Non-zero start is reflected in the index bounds.
	if ti.StartNS != 2_000_000_000 {
		t.Fatalf("start = %d", ti.StartNS)
	}
}

func TestNoKeyframesAndUnknownTrack(t *testing.T) {
	tl := buildTimeline(t, []int64{0, 100, 200}, nil, 100)
	idx, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	res, err := idx.QueryTrack("cam", 150_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hit {
		t.Fatalf("track with no keyframes reported a hit: %+v", res)
	}
	if _, err := idx.QueryTrack("nope", 0); !model.IsKind(err, model.KindNotFound) {
		t.Fatalf("unknown track error = %v", err)
	}
}

func TestKeyframesBetweenHalfOpen(t *testing.T) {
	starts := []int64{0, 100, 200, 300}
	tl := buildTimeline(t, starts, map[int]bool{1: true, 2: true, 3: true}, 100)
	idx, _ := Build(tl)
	ti := idx.Tracks["cam"]
	got := ti.KeyframesBetween(2_100_000_000, 2_300_000_000)
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("between = %+v", got)
	}
	if global := idx.All; len(global) != 3 || global[0].Seq != 1 {
		t.Fatalf("global index = %+v", global)
	}
}
