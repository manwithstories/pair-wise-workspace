package resume

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
)

func validChecksum() string {
	h := sha256.Sum256([]byte("input-bytes"))
	return hex.EncodeToString(h[:])
}

func sampleCheckpoint() *Checkpoint {
	return &Checkpoint{
		InputPath:   "in.mp4",
		InputSha256: validChecksum(),
		Tracks: []TrackState{
			{ID: 1, Kind: int(1), Timescale: 30000, Duration: 90000},
			{ID: 2, Kind: int(2), Timescale: 48000, Duration: 144000,
				Channels: 2, Rate: 48000},
		},
		Segments: []SegmentState{
			{Index: 0, Output: "out/seg-000.mp4", Sha256: "a", StartMillis: 0, EndMillis: 1000},
			{Index: 1, Output: "out/seg-001.mp4", Sha256: "b", StartMillis: 1000, EndMillis: 2000},
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp := sampleCheckpoint()
	if err := store.Save(cp); err != nil {
		t.Fatal(err)
	}
	// No temp file may remain after the atomic commit.
	if _, err := os.Stat(store.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp checkpoint left behind")
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InputSha256 != cp.InputSha256 || len(loaded.Segments) != 2 {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	if loaded.Schema != schemaVersion {
		t.Fatalf("schema = %d, want %d", loaded.Schema, schemaVersion)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	cp, err := store.Load()
	if err != nil || cp != nil {
		t.Fatalf("missing checkpoint should be (nil,nil), got %v %v", cp, err)
	}
}

func TestLoadCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load()
	if !clock.IsKind(err, clock.KindInconsistent) {
		t.Fatalf("want inconsistent for corrupt checkpoint, got %v", err)
	}
}

func TestSaveRejectsBadChecksum(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	cp := sampleCheckpoint()
	cp.InputSha256 = "short"
	if err := store.Save(cp); err == nil {
		t.Fatal("checkpoint with bad checksum saved")
	}
}

func TestVerifyChecksumAndTracks(t *testing.T) {
	cp := sampleCheckpoint()
	store, _ := NewStore(t.TempDir())
	// Matching checksum and tracks.
	if err := store.Verify(cp, cp.InputSha256, cp.Tracks); err != nil {
		t.Fatalf("matching state rejected: %v", err)
	}
	// Input changed (retransmission produced different bytes).
	if err := store.Verify(cp, "f"+cp.InputSha256[1:], cp.Tracks); !clock.IsKind(err, clock.KindInconsistent) {
		t.Fatalf("checksum mismatch not flagged: %v", err)
	}
	// Track timeline changed.
	changed := append([]TrackState(nil), cp.Tracks...)
	changed[0].Duration = 1
	if err := store.Verify(cp, cp.InputSha256, changed); !clock.IsKind(err, clock.KindInconsistent) {
		t.Fatalf("timeline mismatch not flagged: %v", err)
	}
	// Track count changed.
	if err := store.Verify(cp, cp.InputSha256, changed[:1]); !clock.IsKind(err, clock.KindInconsistent) {
		t.Fatalf("track count mismatch not flagged: %v", err)
	}
}

func writeSegment(t *testing.T, dir, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, sum
}

func TestRollbackToLastValidBoundary(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p0, h0 := writeSegment(t, outDir, "seg-000.mp4", "zero")
	p1, h1 := writeSegment(t, outDir, "seg-001.mp4", "one")
	// Segment 2 was interrupted: its file exists but with wrong bytes.
	p2, _ := writeSegment(t, outDir, "seg-002.mp4", "partial")
	cp := sampleCheckpoint()
	cp.Segments = []SegmentState{
		{Index: 0, Output: p0, Sha256: h0},
		{Index: 1, Output: p1, Sha256: h1},
		{Index: 2, Output: p2, Sha256: "deadbeef"}, // recorded hash won't match
	}
	store, _ := NewStore(dir)
	kept := store.Rollback(cp)
	if kept != 2 {
		t.Fatalf("rollback kept %d, want 2", kept)
	}
	if len(cp.Segments) != 2 || cp.Segments[1].Index != 1 {
		t.Fatalf("rollback boundary wrong: %+v", cp.Segments)
	}
}

func TestRollbackMissingFile(t *testing.T) {
	dir := t.TempDir()
	p0, h0 := writeSegment(t, dir, "seg-000.mp4", "ok")
	cp := sampleCheckpoint()
	cp.Segments = []SegmentState{
		{Index: 0, Output: p0, Sha256: h0},
		{Index: 1, Output: filepath.Join(dir, "gone.mp4"), Sha256: "x"},
	}
	store, _ := NewStore(dir)
	if kept := store.Rollback(cp); kept != 1 {
		t.Fatalf("missing-file rollback kept %d, want 1", kept)
	}
}

func TestRollbackGapInIndices(t *testing.T) {
	dir := t.TempDir()
	p0, h0 := writeSegment(t, dir, "seg-000.mp4", "ok")
	cp := sampleCheckpoint()
	cp.Segments = []SegmentState{
		{Index: 0, Output: p0, Sha256: h0},
		{Index: 2, Output: p0, Sha256: h0}, // non-contiguous index
	}
	store, _ := NewStore(dir)
	if kept := store.Rollback(cp); kept != 1 {
		t.Fatalf("gap rollback kept %d, want 1", kept)
	}
}

func TestHashFileDeterministic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := HashFile(path)
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash unstable or wrong length: %s %s", h1, h2)
	}
}
