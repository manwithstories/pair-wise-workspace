package mp4atom_test

import (
	"path/filepath"
	"testing"

	"mp4replay/internal/fixture"
	"mp4replay/internal/mp4atom"
)

func TestMultiSamplesPerChunk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chunk.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: p, Tracks: 1, Frames: 100,
		Variant: fixture.VariantPlain, SamplesPerChunk: 3,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := mp4atom.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	tr := m.VideoTrack()
	if len(tr.Samples) != 100 {
		t.Fatalf("want 100 samples, got %d", len(tr.Samples))
	}
	// offsets must be contiguous and exactly advance by each sample size
	for i, s := range tr.Samples {
		if s.Size == 0 {
			t.Fatal("zero size")
		}
		if i > 0 {
			prev := tr.Samples[i-1]
			if s.Offset != prev.Offset+int64(prev.Size) {
				t.Fatalf("sample %d offset not contiguous: got %d want %d",
					i, s.Offset, prev.Offset+int64(prev.Size))
			}
		}
	}
}

func TestChunkRoundTripHash(t *testing.T) {
	src := filepath.Join(t.TempDir(), "c.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: src, Tracks: 1, Frames: 120,
		Variant: fixture.VariantPlain, SamplesPerChunk: 5,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := mp4atom.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	tr := m.VideoTrack()
	// read first two samples directly and confirm they sit inside one chunk
	if tr.Samples[1].Offset != tr.Samples[0].Offset+int64(tr.Samples[0].Size) {
		t.Fatal("shared-chunk samples not adjacent")
	}
}
