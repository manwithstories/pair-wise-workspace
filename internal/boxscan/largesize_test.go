package boxscan

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestLargeSizeAndZeroSize builds a minimal file using a 64-bit largesize box
// and a zero-size terminal box to exercise both header forms.
func TestLargeSizeAndZeroSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ls.mp4")
	var f []byte
	// ftyp with 64-bit largesize header
	payload := []byte("isom")
	f = append(f, 0, 0, 0, 1)
	f = append(f, []byte("ftyp")...)
	var ls [8]byte
	binary.BigEndian.PutUint64(ls[:], uint64(16+len(payload)))
	f = append(f, ls[:]...)
	f = append(f, payload...)
	// terminal free box with size 0 (extends to EOF)
	f = append(f, 0, 0, 0, 0)
	f = append(f, []byte("free")...)
	f = append(f, 1, 2, 3, 4)
	if err := os.WriteFile(p, f, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer got.Close()
	if len(got.Roots) != 2 {
		t.Fatalf("want 2 roots, got %d", len(got.Roots))
	}
	if got.Roots[0].Header != 16 {
		t.Fatalf("want 16-byte largesize header, got %d", got.Roots[0].Header)
	}
	if got.Roots[1].End != got.Size {
		t.Fatal("zero-size box must extend to EOF")
	}
}

func TestUUIDBox(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "u.mp4")
	var f []byte
	f = append(f, 0, 0, 0, 32)
	f = append(f, []byte("uuid")...)
	f = append(f, make([]byte, 16)...) // extended type
	f = append(f, make([]byte, 8)...)  // payload
	if err := os.WriteFile(p, f, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Open(p)
	if err != nil {
		t.Fatalf("uuid open: %v", err)
	}
	if got.Roots[0].Header != 24 || len(got.Roots[0].UUID) != 16 {
		t.Fatalf("bad uuid box: header=%d uuid=%d",
			got.Roots[0].Header, len(got.Roots[0].UUID))
	}
}
