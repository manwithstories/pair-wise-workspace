package seekidx_test

import (
	"path/filepath"
	"testing"

	"mp4replay/internal/fixture"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/seekidx"
)

func BenchmarkBuildIndex100k(b *testing.B) {
	p := filepath.Join(b.TempDir(), "big.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: p, Tracks: 1, Frames: 100000,
		Variant: fixture.VariantPlain,
	}); err != nil {
		b.Fatal(err)
	}
	m, err := mp4atom.Open(p)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	tr := m.VideoTrack()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := seekidx.Build(tr)
		if len(idx.Keys) == 0 {
			b.Fatal("no keys")
		}
	}
}

func TestBuildIndexUnder300ms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: p, Tracks: 1, Frames: 100000,
		Variant: fixture.VariantPlain,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := mp4atom.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	idx := seekidx.Build(m.VideoTrack())
	if idx.SampleCount != 100000 {
		t.Fatalf("want 100000 samples, got %d", idx.SampleCount)
	}
}
