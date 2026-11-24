package repair

import (
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
	"mediaflow/internal/gen"
)

func setup(t *testing.T) (*cmaf.Scanner, *clock.Timeline) {
	t.Helper()
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	sc := cmaf.NewScanner(cmaf.DefaultLimits())
	initSeg, err := sc.ParseInit(mux.Init(), 0)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	sc.ApplyInit(initSeg)
	tl := clock.NewTimeline()
	tl.Add(clock.TrackClock{TrackID: 1, Timescale: 90000})
	return sc, tl
}

func TestRecoverCleanRepresentation(t *testing.T) {
	sc, tl := setup(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	var segs []cmaf.MediaSegment
	for i := 0; i < 5; i++ {
		b, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: uint32(i + 1), BaseTicks: uint64(i) * 90000, Samples: 30, Keyframes: []int{0}})
		ms, err := sc.ParseMedia(b, i+1)
		if err != nil {
			t.Fatalf("ParseMedia: %v", err)
		}
		segs = append(segs, ms)
	}
	rec := RecoverTrack(tl, 1, segs, DefaultOptions())
	if rec.Recovered != 5 || rec.MissingCount != 0 || rec.Partial != 0 {
		t.Fatalf("recovery = %+v", rec)
	}
	if len(rec.Missing) != 0 {
		t.Fatalf("unexpected missing: %+v", rec.Missing)
	}
	start, end, ok := rec.Boundary()
	if !ok || start != 0 || end != 5*90000*1_000_000/90000 {
		t.Fatalf("boundary = %d..%d ok=%v", start, end, ok)
	}
	if !rec.Index.Valid() {
		t.Fatalf("index should be valid: %+v", rec.Index)
	}
	if rec.Digest == "" {
		t.Fatal("empty digest")
	}
	// Re-running the identical input yields the identical digest.
	again := RecoverTrack(tl, 1, segs, DefaultOptions())
	if again.Digest != rec.Digest {
		t.Fatalf("digest not deterministic: %s vs %s", rec.Digest, again.Digest)
	}
}

func TestRecoverDuplicateAndGap(t *testing.T) {
	sc, tl := setup(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	mk := func(seq uint32, base uint64) cmaf.MediaSegment {
		b, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: seq, BaseTicks: base, Samples: 30, Keyframes: []int{0}})
		ms, err := sc.ParseMedia(b, int(seq))
		if err != nil {
			t.Fatalf("ParseMedia: %v", err)
		}
		return ms
	}
	// seq 1, then 3 (gap), then a duplicate of 3.
	segs := []cmaf.MediaSegment{mk(1, 0), mk(3, 180000), mk(3, 180000)}
	rec := RecoverTrack(tl, 1, segs, DefaultOptions())
	if rec.Duplicates != 1 {
		t.Fatalf("duplicates = %d", rec.Duplicates)
	}
	if rec.Untrusted != 0 {
		t.Fatalf("duplicates must not inflate untrusted: %d", rec.Untrusted)
	}
	if rec.Missing.TotalMS() <= 0 {
		t.Fatalf("gap should be reported as missing: %+v", rec.Missing)
	}
	if rec.Index.Valid() {
		t.Fatalf("index with duplicates/missing should not be valid")
	}
	// The duplicate plan is excluded from recoverable count.
	if len(rec.Fragments) != 3 {
		t.Fatalf("fragments = %+v", rec.Fragments)
	}
	lastPlan := rec.Fragments[len(rec.Fragments)-1]
	if lastPlan.Status != StatusDuplicate || lastPlan.DuplicateOf != 3 {
		t.Fatalf("expected duplicate plan, got %+v", lastPlan)
	}
}

func TestRecoverTfdtRegressionReanchors(t *testing.T) {
	sc, tl := setup(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	mk := func(seq uint32, base uint64) cmaf.MediaSegment {
		b, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: seq, BaseTicks: base, Samples: 10})
		ms, err := sc.ParseMedia(b, int(seq))
		if err != nil {
			t.Fatalf("ParseMedia: %v", err)
		}
		return ms
	}
	segs := []cmaf.MediaSegment{mk(1, 90000), mk(2, 0)} // second regresses to 0
	rec := RecoverTrack(tl, 1, segs, DefaultOptions())
	last := rec.Fragments[len(rec.Fragments)-1]
	if last.Trusted {
		t.Fatalf("regressed fragment should be untrusted: %+v", last)
	}
	foundReg := false
	for _, e := range rec.Errors {
		if e.Kind == clock.ErrTfdtRegression {
			foundReg = true
		}
	}
	if !foundReg {
		t.Fatalf("expected tfdt regression error: %+v", rec.Errors)
	}
}

func TestRecoverTruncated(t *testing.T) {
	sc, tl := setup(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	full, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: 1, Samples: 30})
	cut := full[:len(full)-2000]
	ms, err := sc.ParseMedia(cut, 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	rec := RecoverTrack(tl, 1, []cmaf.MediaSegment{ms}, DefaultOptions())
	if rec.Partial == 0 && rec.MissingCount == 0 {
		t.Fatalf("expected partial/missing recovery: %+v", rec)
	}
	if len(rec.Missing) == 0 {
		t.Fatalf("expected missing windows: %+v", rec)
	}
	// Unrecoverable windows must be explicitly missing, not counted as good.
	for _, f := range rec.Fragments {
		if f.DroppedSamples > 0 && f.Status == StatusComplete {
			t.Fatalf("truncated fragment marked complete: %+v", f)
		}
	}
}

func TestRecomputeSegmentIndex(t *testing.T) {
	sc, tl := setup(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	b, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: 1, Samples: 30})
	ms, err := sc.ParseMedia(b, 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	entries := RecomputeSegment(tl, 1, ms)
	if len(entries) != 1 || entries[0].Source != SourceFragment || !entries[0].Trusted {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestCrossCheckBoundaries(t *testing.T) {
	layers := []LayerResult{
		{Name: "360p", Index: 0, StartUs: 0, EndUs: 3_000_000, Digest: "a"},
		{Name: "720p", Index: 1, StartUs: 10_000, EndUs: 2_990_000, Digest: "b"},
		{Name: "1080p", Index: 2, StartUs: 0, EndUs: 3_000_000, Digest: "a"},
	}
	// 360p and 1080p share a digest, so the majority reference is 360p.
	ref, checks, mismatches := CrossCheck(layers, 40)
	if ref != "360p" {
		t.Fatalf("reference = %s", ref)
	}
	if len(checks) != 3 || mismatches != 0 {
		t.Fatalf("checks = %+v mismatches=%d", checks, mismatches)
	}
	if got := CrossCheckMaxMS(checks); got != 10 {
		t.Fatalf("max delta = %v", got)
	}
	// A one-frame boundary difference exceeds a 5ms tolerance.
	_, _, tight := CrossCheck(layers, 5)
	if tight != 1 {
		t.Fatalf("tight tolerance mismatches = %d", tight)
	}
	// A failed layer is excluded and cannot rewrite others.
	layers = append(layers, LayerResult{Name: "broken", Failed: true})
	_, checks2, _ := CrossCheck(layers, 40)
	for _, c := range checks2 {
		if c.Layer == "broken" {
			t.Fatal("failed layer must be skipped")
		}
	}
}
