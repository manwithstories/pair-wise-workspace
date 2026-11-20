package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"archive/internal/model"
)

func TestChunkRoundTripAndChecksum(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 25; i++ {
		if err := store.Add(KeyframeEntry{SSRC: 1, Sequence: i, TimeNanos: i * 1000,
			FrameIndex: i, Segment: 0}, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FlushTail(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveManifest("in", "params"); err != nil {
		t.Fatal(err)
	}
	res, err := VerifyAndResume(dir, "in", "params")
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	// 25 packets at 10/chunk => 2 complete chunks + 1 incomplete tail.
	if res.CompleteChunks != 2 {
		t.Errorf("complete chunks = %d, want 2", res.CompleteChunks)
	}
	if res.RecoveredChunks != 2 {
		t.Errorf("recovered chunks = %d, want 2", res.RecoveredChunks)
	}
}

func TestResumeRejectsInputDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir, 10)
	for i := int64(0); i < 12; i++ {
		_ = store.Add(KeyframeEntry{Sequence: i}, i)
	}
	_ = store.FlushTail()
	_ = store.SaveManifest("original", "p")
	_, err := VerifyAndResume(dir, "tampered", "p")
	if err == nil {
		t.Fatal("expected input digest mismatch error")
	}
	if me := model.AsError(err); me == nil || me.Kind != model.KindInput {
		t.Fatalf("expected KindInput, got %v", err)
	}
}

func TestResumeRejectsParamDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir, 10)
	for i := int64(0); i < 12; i++ {
		_ = store.Add(KeyframeEntry{Sequence: i}, i)
	}
	_ = store.FlushTail()
	_ = store.SaveManifest("in", "original-params")
	if _, err := VerifyAndResume(dir, "in", "changed-params"); err == nil {
		t.Fatal("expected parameter digest mismatch error")
	}
}

func TestCorruptChunkDetected(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir, 10)
	for i := int64(0); i < 20; i++ {
		_ = store.Add(KeyframeEntry{Sequence: i}, i)
	}
	_ = store.SaveManifest("in", "p")
	// Tamper with a checksum-covered entry field inside the first chunk file.
	first := filepath.Join(dir, chunkName(0))
	data, _ := os.ReadFile(first)
	tampered := bytes.Replace(data, []byte("\"sequence\": 2"), []byte("\"sequence\": 7777"), 1)
	if bytes.Equal(tampered, data) {
		t.Fatalf("test fixture changed; sequence field not found in %s", first)
	}
	if err := os.WriteFile(first, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAndResume(dir, "in", "p"); err == nil {
		t.Fatal("expected checksum corruption to be detected")
	}
}

func TestIncompleteTailRebuiltNotTrusted(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir, 10)
	for i := int64(0); i < 23; i++ { // 2 complete + partial
		_ = store.Add(KeyframeEntry{Sequence: i}, i)
	}
	_ = store.FlushTail()
	_ = store.SaveManifest("in", "p")
	m, _ := LoadManifest(dir)
	if CountCompleteChunks(m) != 2 {
		t.Errorf("complete = %d, want 2", CountCompleteChunks(m))
	}
	res, err := VerifyAndResume(dir, "in", "p")
	if err != nil {
		t.Fatalf("valid complete chunks should resume despite tail: %v", err)
	}
	if res.CompleteChunks != 2 {
		t.Errorf("resume complete chunks = %d, want 2", res.CompleteChunks)
	}
}

func TestFreshDirReturnsNoResume(t *testing.T) {
	res, err := VerifyAndResume(t.TempDir(), "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if res.Available {
		t.Error("empty directory must not report an available resume")
	}
}
