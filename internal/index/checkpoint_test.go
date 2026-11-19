package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"archive/internal/errs"
)

func TestSaveLoadVerify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.index.json")
	cp := New("asset-a", "hash-1", 1024)
	cp.Chunks = append(cp.Chunks, Chunk{Index: 0, ByteEnd: 1024, InputPos: 3, SHA256: "abc"})
	if err := Save(path, cp); err != nil {
		t.Fatal(err)
	}
	// atomic: no tmp file left behind
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file not cleaned up")
	}
	got, err := Load(path)
	if err != nil || got == nil {
		t.Fatalf("load %v %+v", err, got)
	}
	if err := got.Verify("asset-a", "hash-1"); err != nil {
		t.Fatal(err)
	}
	if err := got.Verify("asset-a", "hash-2"); err == nil {
		t.Fatal("input hash change accepted")
	}
	got.Version = 99
	if err := got.Verify("asset-a", "hash-1"); err == nil {
		t.Fatal("bad version accepted")
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if cp, err := Load(filepath.Join(dir, "none")); cp != nil || err != nil {
		t.Fatalf("missing file: %+v %v", cp, err)
	}
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("{not json"), 0o644)
	_, err := Load(bad)
	if err == nil {
		t.Fatal("corrupt index accepted")
	}
	if e, ok := err.(*errs.Error); !ok || e.Kind != errs.KindIndex {
		t.Fatalf("wrong kind: %v", err)
	}
}

func TestFirstInvalidChunkAndResume(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 256) // 4096 bytes
	cp := New("a", "h", 1024)
	for i := 0; i < 4; i++ {
		h, err := HashRange(bytes.NewReader(payload), int64(i*1024), int64((i+1)*1024))
		if err != nil {
			t.Fatal(err)
		}
		cp.Chunks = append(cp.Chunks, Chunk{Index: i, ByteEnd: int64((i + 1) * 1024), InputPos: i + 1, SHA256: h})
	}
	r := bytes.NewReader(payload)
	if n := cp.FirstInvalidChunk(r, int64(len(payload))); n != 4 {
		t.Fatalf("valid=%d", n)
	}
	// corrupt byte inside chunk 2
	payload[2500] ^= 0xff
	if n := cp.FirstInvalidChunk(bytes.NewReader(payload), int64(len(payload))); n != 2 {
		t.Fatalf("valid=%d, want 2", n)
	}
	trunc, pos := cp.ResumePoint(2)
	if trunc != 2048 || pos != 2 {
		t.Fatalf("resume (%d,%d)", trunc, pos)
	}
	// truncated output file
	if n := cp.FirstInvalidChunk(bytes.NewReader(payload[:1500]), 1500); n != 1 {
		t.Fatalf("valid=%d, want 1", n)
	}
}
