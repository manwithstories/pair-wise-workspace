package fingerprint

import (
	"testing"

	"mediad/internal/model"
)

func TestSegmentHashSensitiveToBytesAndRange(t *testing.T) {
	seg := model.Segment{ID: "s1", TrackID: "trk-1", StartTick: 0, EndTick: 100, Status: model.StatusOK}
	h1 := SegmentHash([]byte("payload-A"), seg)
	h2 := SegmentHash([]byte("payload-B"), seg)
	if h1 == h2 {
		t.Fatal("different bytes must produce different hashes")
	}
	moved := seg
	moved.StartTick = 50
	if SegmentHash([]byte("payload-A"), moved) == h1 {
		t.Fatal("time range change must change the hash")
	}
	otherTrack := seg
	otherTrack.TrackID = "trk-2"
	if SegmentHash([]byte("payload-A"), otherTrack) == h1 {
		t.Fatal("track change must change the hash")
	}
	// 确定性：相同输入重复计算一致。
	if SegmentHash([]byte("payload-A"), seg) != h1 {
		t.Fatal("hash must be deterministic")
	}
	if !Verify([]byte("payload-A"), seg, h1) {
		t.Fatal("Verify must accept the matching digest")
	}
	if Verify([]byte("tampered"), seg, h1) {
		t.Fatal("Verify must reject tampered bytes")
	}
}

func TestSourceDigestOrderSensitive(t *testing.T) {
	a := []model.Source{{ID: "a", Kind: model.KindMP4, Digest: "da"}, {ID: "b", Kind: model.KindMP4, Digest: "db"}}
	b := []model.Source{a[1], a[0]}
	if SourceDigest(a) == SourceDigest(b) {
		t.Fatal("source order must matter")
	}
	if SourceDigest(a) != SourceDigest(a) {
		t.Fatal("identical inputs must hash identically")
	}
	// 任一来源摘要变化即失效。
	c := []model.Source{a[0], {ID: "b", Kind: model.KindMP4, Digest: "changed"}}
	if SourceDigest(a) == SourceDigest(c) {
		t.Fatal("changed source digest must change combined digest")
	}
}

func TestFileDigest(t *testing.T) {
	d1 := FileDigest([]byte("file contents"))
	d2 := FileDigest([]byte("file contents"))
	d3 := FileDigest([]byte("file contents!"))
	if d1 != d2 || d1 == d3 || len(d1) != 64 {
		t.Fatalf("digest stability/uniqueness failed: %s %s", d1, d3)
	}
}
