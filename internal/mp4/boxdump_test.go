package mp4

import (
	"os"
	"testing"

	"medialine/internal/fault"
	"medialine/internal/testkit"
)

// TestBoxDump prints the generated box tree to exercise the structural walk.
func TestBoxDump(t *testing.T) {
	dir := t.TempDir()
	spec := testkit.ClipSpec{Name: "clip", Seed: 1, Tracks: []testkit.TrackSpec{
		testkit.DefaultVideoTrack(1, 1_000_000, 15),
	}}
	c, err := testkit.GenerateClip(dir, spec)
	if err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	f, err := Parse(c.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Moov.Size == 0 || f.Mdat.Size == 0 || f.Mfra.Size == 0 {
		t.Fatalf("missing spans: %+v", f)
	}
	if _, ok := fault.As(nil); ok {
		t.Fatal("nil should not be a fault")
	}
}
