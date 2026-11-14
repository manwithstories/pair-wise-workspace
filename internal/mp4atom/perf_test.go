package mp4atom_test

import (
	"path/filepath"
	"testing"
	"time"

	"mp4replay/internal/fixture"
	"mp4replay/internal/mp4atom"
)

func TestOpen100kUnder300ms(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	p := filepath.Join(t.TempDir(), "big.mp4")
	if err := fixture.Generate(fixture.Options{
		Path: p, Tracks: 1, Frames: 100000,
		Variant: fixture.VariantPlain,
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m, err := mp4atom.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	elapsed := time.Since(start)
	if len(m.VideoTrack().Samples) != 100000 {
		t.Fatal("sample count mismatch")
	}
	t.Logf("open+parse+expand 100k samples took %v", elapsed)
	if elapsed > 300*time.Millisecond {
		t.Fatalf("open took %v, want <300ms", elapsed)
	}
}
