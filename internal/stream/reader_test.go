package stream

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadBounded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := ReadBounded(p, 5)
	if err != nil || string(data) != "hello" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if _, err := ReadBounded(p, 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if _, err := ReadBounded(filepath.Join(dir, "missing"), 10); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("payload")
	sum := SHA256Hex(data)
	if len(sum) != 64 {
		t.Fatalf("digest length %d", len(sum))
	}
	if !VerifySHA256(data, sum) {
		t.Fatal("expected match")
	}
	if VerifySHA256([]byte("other"), sum) {
		t.Fatal("expected mismatch")
	}
}
