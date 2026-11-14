package mp4atom_test

import (
	"path/filepath"
	"testing"

	"mp4replay/internal/fixture"
	"mp4replay/internal/mp4atom"
)

func TestCttsV0(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v0.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: p, Tracks: 1, Frames: 100,
		Variant: fixture.VariantElstCttsWrap, CttsV0: true,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := mp4atom.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	tr := m.VideoTrack()
	if !tr.Table.HasCtts || tr.Table.CttsVer != 0 {
		t.Fatalf("want ctts v0, got ver=%d has=%v",
			tr.Table.CttsVer, tr.Table.HasCtts)
	}
}

func TestUnknownBoxesSkippedAndCounted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "u.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: p, Tracks: 1, Frames: 40, Variant: fixture.VariantPlain,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.InjectUnknownBoxes(p, 3); err != nil {
		t.Fatal(err)
	}
	m, err := mp4atom.Open(p)
	if err != nil {
		t.Fatalf("unknown boxes must not break parsing: %v", err)
	}
	defer m.Close()
	if m.UnknownSkip < 3 {
		t.Fatalf("want >=3 unknown skipped, got %d", m.UnknownSkip)
	}
}

func TestSingleAndMultiTrack(t *testing.T) {
	for _, n := range []int{1, 2} {
		p := filepath.Join(t.TempDir(), "t.mp4")
		if err := fixture.Generate(fixture.Options{
			Path: p, Tracks: n, Frames: 30, Variant: fixture.VariantPlain,
		}); err != nil {
			t.Fatal(err)
		}
		m, err := mp4atom.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Tracks) != n {
			t.Fatalf("want %d tracks, got %d", n, len(m.Tracks))
		}
		m.Close()
	}
}
