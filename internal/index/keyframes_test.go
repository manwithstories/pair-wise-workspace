package index

import (
	"testing"

	"mediadeliver/internal/model"
)

func pkt(src string, tid uint32, seq, dts int64, key bool) *model.Packet {
	return &model.Packet{SourceID: src, TrackID: tid, Seq: seq, DTS: dts,
		PTS: dts, Timescale: 90000, Keyframe: key}
}

func TestOnlyKeyframesBecomeCutPoints(t *testing.T) {
	idx := New()
	// keyframes only at seq 0 and 8 (sparse).
	for s := int64(0); s < 12; s++ {
		idx.Add(pkt("cam1", 1, s, s*1000, s == 0 || s == 8))
	}
	idx.Finalize()
	pts := idx.Points("cam1", 1)
	if len(pts) != 2 {
		t.Fatalf("want 2 cut points, got %d", len(pts))
	}
	if pts[0].Seq != 0 || pts[1].Seq != 8 {
		t.Fatalf("cut points must be the real keyframes: %+v", pts)
	}
}

func TestLookupNearestKeyframeAtOrBefore(t *testing.T) {
	idx := New()
	for s := int64(0); s < 12; s++ {
		idx.Add(pkt("cam1", 1, s, s*1000, s == 0 || s == 8))
	}
	idx.Finalize()

	// Exact hit.
	cp, ok := idx.Lookup("cam1", 1, 8000)
	if !ok || cp.Seq != 8 {
		t.Fatalf("exact lookup: %+v ok=%v", cp, ok)
	}
	// Between keyframes must snap back to the earlier keyframe, never forward
	// onto a non-keyframe.
	cp, ok = idx.Lookup("cam1", 1, 9999)
	if !ok || cp.Seq != 8 {
		t.Fatalf("snap-back lookup: %+v ok=%v", cp, ok)
	}
	cp, ok = idx.Lookup("cam1", 1, 7999)
	if !ok || cp.Seq != 0 {
		t.Fatalf("should snap to seq 0, got %+v ok=%v", cp, ok)
	}
	// Before the first keyframe is not addressable.
	if _, ok := idx.Lookup("cam1", 1, -1); ok {
		t.Fatal("timestamp before first keyframe must not be addressable")
	}
	// Unknown track.
	if _, ok := idx.Lookup("camX", 9, 100); ok {
		t.Fatal("unknown track must not be addressable")
	}
}

func TestNoFalseCutPointWhenNoKeyframes(t *testing.T) {
	idx := New()
	for s := int64(0); s < 5; s++ {
		idx.Add(pkt("cam1", 1, s, s, false)) // no keyframes at all
	}
	idx.Finalize()
	if _, ok := idx.Lookup("cam1", 1, 3); ok {
		t.Fatal("stream without keyframes must report no addressable point")
	}
	if got := idx.OverallHitRate(); got != 0 {
		t.Fatalf("hit rate without keyframes: %v", got)
	}
}

func TestHitRates(t *testing.T) {
	idx := New()
	for s := int64(0); s < 10; s++ {
		idx.Add(pkt("cam1", 1, s, s, s%2 == 0)) // 5 of 10
	}
	if got := idx.HitRate("cam1", 1); got != 0.5 {
		t.Fatalf("track hit rate: %v", got)
	}
	if got := idx.OverallHitRate(); got != 0.5 {
		t.Fatalf("overall hit rate: %v", got)
	}
	total, keys := idx.Counts("cam1", 1)
	if total != 10 || keys != 5 {
		t.Fatalf("counts: total=%d keys=%d", total, keys)
	}
}

func TestPerTrackIsolation(t *testing.T) {
	idx := New()
	idx.Add(pkt("cam1", 1, 0, 0, true))
	idx.Add(pkt("cam2", 2, 0, 0, false))
	idx.Finalize()
	if pts := idx.Points("cam1", 1); len(pts) != 1 {
		t.Fatal("cam1 keyframe missing")
	}
	if pts := idx.Points("cam2", 2); len(pts) != 0 {
		t.Fatal("cam2 non-keyframe must not be indexed")
	}
}
