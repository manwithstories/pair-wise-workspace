package probe

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// craftBox writes a single box header claiming a huge payload.
func craftBox(t *testing.T, path string, size uint64, typ string, body []byte) {
	t.Helper()
	var hdr [16]byte
	binary.BigEndian.PutUint32(hdr[:4], 1)
	copy(hdr[4:8], typ)
	binary.BigEndian.PutUint64(hdr[8:], size)
	if err := os.WriteFile(path, append(hdr[:], body...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHugeDeclaredBoxDoesNotAllocate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.mp4")
	craftBox(t, path, 1<<40, "moov", nil)
	f, err := Analyze(path, DefaultConfig())
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if !f.Truncated {
		t.Fatal("oversized box should be flagged truncated")
	}
	if len(f.Boxes) != 1 || f.Boxes[0].Size != 16 {
		t.Fatalf("boxes=%+v", f.Boxes)
	}
}

func TestNegativeAndZeroSizes(t *testing.T) {
	dir := t.TempDir()
	// A zero size means "to EOF" per spec and must not loop forever.
	zero := filepath.Join(dir, "zero.mp4")
	if err := os.WriteFile(zero, []byte{0, 0, 0, 0, 'm', 'd', 'a', 't', 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Analyze(zero, DefaultConfig())
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(f.Boxes) != 1 || f.Boxes[0].Type != "mdat" {
		t.Fatalf("boxes=%+v", f.Boxes)
	}
	// Size below the 8-byte header is a structural error.
	bad := filepath.Join(dir, "bad.mp4")
	if err := os.WriteFile(bad, []byte{0, 0, 0, 4, 'f', 't', 'y', 'p'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze(bad, DefaultConfig()); err == nil {
		t.Fatal("expected structural error for undersized box")
	}
}

func TestMoovCapLimitsAllocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big_moov.mp4")
	// moov declares 1 MiB but only 32 KiB exists in the file.
	craftBox(t, path, 1<<20, "moov", make([]byte, 32<<10))
	cfg := DefaultConfig()
	cfg.MaxMoovBytes = 4096
	cfg.MaxDecodeBytes = 4096
	f, err := Analyze(path, cfg)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if !f.Truncated && len(f.Issues) == 0 {
		t.Fatal("expected a truncation or structure fault")
	}
}

func TestConfigNormalizeBounds(t *testing.T) {
	cfg := Config{ReadBuffer: 1 << 30, MaxDecodeBytes: 1 << 40, MaxMoovBytes: 1024}.Normalize()
	if cfg.ReadBuffer > 8<<20 {
		t.Fatalf("ReadBuffer=%d not capped", cfg.ReadBuffer)
	}
	if cfg.MaxDecodeBytes > cfg.MaxMoovBytes {
		t.Fatalf("MaxDecodeBytes=%d exceeds moov cap %d", cfg.MaxDecodeBytes, cfg.MaxMoovBytes)
	}
	def := Config{}.Normalize()
	if def.SegmentMS != 2000 || def.MaxSamples == 0 {
		t.Fatalf("defaults not applied: %+v", def)
	}
}

func TestSampleReaderRejectsOutOfRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(path, make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := newSampleReader(f, 32, 8)
	if _, err := r.read(-1, 4); err == nil {
		t.Fatal("negative offset accepted")
	}
	if _, err := r.read(30, 4); err == nil {
		t.Fatal("overrunning read accepted")
	}
	if _, err := r.read(0, 9); err == nil {
		t.Fatal("oversized read accepted")
	}
	if _, err := r.read(0, 8); err != nil {
		t.Fatalf("valid read: %v", err)
	}
}
