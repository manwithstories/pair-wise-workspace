package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/mediad/internal/ingest"
	"github.com/example/mediad/internal/model"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHashFileDeterministic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.part")
	writeFile(t, p, "segment-bytes")
	h1, n1, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h2, _, _ := HashFile(p)
	if h1 != h2 {
		t.Fatal("repeated hashing must be identical")
	}
	if n1 != int64(len("segment-bytes")) {
		t.Fatalf("size = %d", n1)
	}
	if h1 != HashBytes([]byte("segment-bytes")) {
		t.Fatal("file and byte hashes must agree")
	}
}

func TestRecomputeHashes(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "s1.part")
	p2 := filepath.Join(dir, "s2.part")
	writeFile(t, p1, "one")
	writeFile(t, p2, "two")
	h1, _, _ := HashFile(p1)
	h2, _, _ := HashFile(p2)

	m := &Manifest{Segments: []SegmentRecord{
		{ID: "s1", Path: p1, SHA256: h1},
		{ID: "s2", Path: p2, SHA256: h2},
	}}
	matched, err := RecomputeHashes(m)
	if err != nil || matched != 2 {
		t.Fatalf("matched=%d err=%v", matched, err)
	}

	// Tamper with one file: mismatch must be a checksum error.
	writeFile(t, p2, "tampered")
	_, err = RecomputeHashes(m)
	me, ok := model.AsError(err)
	if !ok || me.Kind != model.KindChecksum {
		t.Fatalf("tamper must fail checksum: %v", err)
	}
}

func TestRecomputeSkipsRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.part")
	writeFile(t, p, "ok")
	h, _, _ := HashFile(p)
	m := &Manifest{Segments: []SegmentRecord{
		{ID: "ok", Path: p, SHA256: h},
		{ID: "bad", Rejected: true, RejectReason: "quarantine"},
	}}
	matched, err := RecomputeHashes(m)
	if err != nil || matched != 1 {
		t.Fatalf("matched=%d err=%v", matched, err)
	}
}

func TestKeyframeHitRate(t *testing.T) {
	planned := []uint32{1, 4, 7, 10}
	// One planned keyframe missing from the actual index.
	actual := []uint32{1, 4, 10}
	if got := KeyframeHitRate(planned, actual); got != 0.75 {
		t.Fatalf("hit rate = %v", got)
	}
	if got := KeyframeHitRate(planned, planned); got != 1.0 {
		t.Fatalf("full hit rate = %v", got)
	}
	if got := KeyframeHitRate(nil, nil); got != 0 {
		t.Fatalf("empty plan = %v", got)
	}
	if p := PlannedKeyframes(12, 3); len(p) != 4 || p[3] != 10 {
		t.Fatalf("planned = %v", p)
	}
}

func TestRoundTripMP4(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rt.mp4")
	if _, err := ingest.GenerateMP4(ingest.FixtureSpec{
		Path: path, Width: 1280, Height: 720, VideoSamples: 9,
		KeyframeEvery: 3,
	}); err != nil {
		t.Fatal(err)
	}
	exp := RoundTripExpectation{
		TrackID: 1, SampleCount: 9, Width: 1280, Height: 720,
		Keyframes: PlannedKeyframes(9, 3),
	}
	if err := RoundTripMP4(path, exp); err != nil {
		t.Fatalf("round trip must match: %v", err)
	}

	// A changed expectation must be detected.
	exp.SampleCount = 99
	if err := RoundTripMP4(path, exp); err == nil {
		t.Fatal("altered expectation must fail round-trip check")
	}
}
