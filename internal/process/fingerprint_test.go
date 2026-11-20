package process

import (
	"testing"
)

func entry(track string, start, end int64, content string) Entry {
	return Entry{TrackID: track, Seg: Segment{Start: start, End: end},
		ContentDigest: DigestSamples([]byte(content))}
}

func TestFingerprintCrossTrack(t *testing.T) {
	a := entry("audio", 1000, 2000, "samples")
	b := entry("video", 1000, 2000, "samples")
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("same window+content across tracks must share fingerprint")
	}
	c := entry("audio", 1000, 2001, "samples")
	if Fingerprint(a) == Fingerprint(c) {
		t.Fatal("different window must differ")
	}
}

func TestDedup(t *testing.T) {
	entries := []Entry{
		entry("audio", 0, 1000, "A"),
		entry("video", 0, 1000, "A"), // 与 1 重复 → 去重
		entry("audio", 1000, 2000, "B"),
		entry("video", 0, 1000, "C"), // 同窗不同内容 → 保留
	}
	res := Dedup(entries)
	if res.Deduplicated != 1 {
		t.Fatalf("dedup=%d", res.Deduplicated)
	}
	if len(res.Kept) != 3 {
		t.Fatalf("kept=%d", len(res.Kept))
	}
	if res.Collisions != 0 {
		t.Fatalf("collisions=%d", res.Collisions)
	}
}

func TestDedupDeterministic(t *testing.T) {
	entries := []Entry{
		entry("t1", 0, 100, "x"),
		entry("t2", 0, 100, "x"),
		entry("t3", 0, 100, "x"),
	}
	r1 := Dedup(entries)
	r2 := Dedup(entries)
	if r1.Deduplicated != 2 || r2.Deduplicated != 2 {
		t.Fatalf("dedup=%d/%d", r1.Deduplicated, r2.Deduplicated)
	}
	if r1.Kept[0].TrackID != "t1" {
		t.Fatalf("kept=%q", r1.Kept[0].TrackID)
	}
}
