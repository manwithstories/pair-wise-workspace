package ingest

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestLocateStructuralDamageFindsTruncatedBoxOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "truncated.mp4")
	b := []byte{0, 0, 0, 16, 'f', 't', 'y', 'p'}
	b = append(b, make([]byte, 8)...)
	sttsOffset := len(b)
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[:4], 128)
	copy(header[4:], "stts")
	b = append(b, header...)
	b = append(b, make([]byte, 8)...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	fe, err := LocateStructuralDamage(path)
	if err != nil {
		t.Fatal(err)
	}
	if fe == nil || fe.Location == nil {
		t.Fatalf("expected structural fault, got %v", fe)
	}
	if fe.Location.Offset != int64(sttsOffset) {
		t.Fatalf("offset=%d want %d", fe.Location.Offset, sttsOffset)
	}
}
