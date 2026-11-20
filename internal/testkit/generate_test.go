package testkit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateDeterministic(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	if _, err := Generate(d1, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(d2, 42); err != nil {
		t.Fatal(err)
	}
	for _, name := range All() {
		b1, err := os.ReadFile(filepath.Join(d1, name))
		if err != nil {
			t.Fatal(err)
		}
		b2, err := os.ReadFile(filepath.Join(d2, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("%s differs between runs", name)
		}
	}
}

func TestGenerateDifferentSeeds(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	Generate(d1, 1)
	Generate(d2, 2)
	b1, _ := os.ReadFile(filepath.Join(d1, GoodSRT))
	b2, _ := os.ReadFile(filepath.Join(d2, GoodSRT))
	if bytes.Equal(b1, b2) {
		t.Fatal("different seeds should differ")
	}
}

func TestFixtureSizes(t *testing.T) {
	dir := t.TempDir()
	fx, err := Generate(dir, 42)
	if err != nil {
		t.Fatal(err)
	}
	if fx.MediaTicks != 30*27000000 {
		t.Fatalf("media ticks=%d", fx.MediaTicks)
	}
	for _, name := range All() {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s empty", name)
		}
		if info.Size() > 1<<20 {
			t.Fatalf("%s too large: %d", name, info.Size())
		}
	}
}
