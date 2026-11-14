package mp4atom_test

import (
	"os"
	"path/filepath"
	"testing"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/fixture"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

func codeOf(err error) int {
	if e, ok := err.(*report.Error); ok {
		return e.Code
	}
	return -1
}

func fixturePath(t *testing.T, frames int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: p, Tracks: 1, Frames: frames,
		Variant: fixture.VariantPlain,
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingMoov(t *testing.T) {
	src := fixturePath(t, 20)
	f, err := boxscan.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	moovOff := boxscan.FindFirst(f.Roots, "moov").Start
	f.Close()
	b, _ := os.ReadFile(src)
	copy(b[moovOff+4:moovOff+8], []byte("free"))
	dst := filepath.Join(t.TempDir(), "nomoov.mp4")
	os.WriteFile(dst, b, 0o644)
	_, err = mp4atom.Open(dst)
	if codeOf(err) != report.ExitBoxInvalid {
		t.Fatalf("want box invalid, got %v", err)
	}
}

func TestFragmentedMoofRejected(t *testing.T) {
	src := fixturePath(t, 20)
	b, _ := os.ReadFile(src)
	// inject a moof box before everything else
	moof := append([]byte{0, 0, 0, 16}, []byte("moof")...)
	moof = append(moof, 0, 0, 0, 8, 't', 'r', 'a', 'f')
	out := append(moof, b...)
	dst := filepath.Join(t.TempDir(), "frag.mp4")
	os.WriteFile(dst, out, 0o644)
	_, err := mp4atom.Open(dst)
	if codeOf(err) != report.ExitBoxInvalid {
		t.Fatalf("want box invalid for moof, got %v", err)
	}
}

func TestFileReadError(t *testing.T) {
	_, err := mp4atom.Open(filepath.Join(t.TempDir(), "nope.mp4"))
	if codeOf(err) != report.ExitFileRead {
		t.Fatalf("want file read code, got %v", err)
	}
}
