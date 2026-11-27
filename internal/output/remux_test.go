package output

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestRemuxerBlockChecksumsAndConcat(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRemuxer(dir, "media")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xAB}, BlockSize+100)
	if _, err := r.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	blocks := r.CommittedBlocks()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	// running digest must equal the digest of the full payload
	want := sha256.Sum256(payload)
	if r.RunningDigest() != hex.EncodeToString(want[:]) {
		t.Fatal("running digest differs from whole-payload digest")
	}
	final := filepath.Join(dir, "final.bin")
	digest, n, err := ConcatBlocks(dir, "media", final, r.BlockCount())
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) || digest != hex.EncodeToString(want[:]) {
		t.Fatalf("concat digest/size mismatch n=%d", n)
	}
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("concatenated file content mismatch")
	}
}

func TestResumeReconstructsRunningHash(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRemuxer(dir, "media")
	first := bytes.Repeat([]byte{0x01}, BlockSize)
	if _, err := r.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitBlock(); err != nil {
		t.Fatal(err)
	}
	blocks := r.CommittedBlocks()

	// simulate a fresh process resuming from the committed block
	r2, _ := NewRemuxer(dir, "media")
	if err := r2.Resume(blocks); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	more := []byte{0x02, 0x03}
	if _, err := r2.Write(more); err != nil {
		t.Fatal(err)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	combined := append(append([]byte{}, first...), more...)
	want := sha256.Sum256(combined)
	if r2.RunningDigest() != hex.EncodeToString(want[:]) {
		t.Fatal("resumed running hash does not match end-to-end digest")
	}
}

func TestResumeRejectsTamperedBlock(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRemuxer(dir, "media")
	if _, err := r.Write(make([]byte, 128)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitBlock(); err != nil {
		t.Fatal(err)
	}
	blocks := r.CommittedBlocks()
	// tamper with the on-disk block after recording its checksum
	p := filepath.Join(dir, "media.0000.blk")
	b, _ := os.ReadFile(p)
	b[0] ^= 0xFF
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	r2, _ := NewRemuxer(dir, "media")
	if err := r2.Resume(blocks); err == nil {
		t.Fatal("tampered block must fail checksum validation")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := NewManifest("abc123")
	m.OutputSHA256 = "def456"
	m.RecoveredPackets = 3
	if err := m.Write(dir); err != nil {
		t.Fatal(err)
	}
	back, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.InputSHA256 != "abc123" || back.OutputSHA256 != "def456" ||
		back.RecoveredPackets != 3 {
		t.Fatalf("manifest round trip mismatch: %+v", back)
	}
}
