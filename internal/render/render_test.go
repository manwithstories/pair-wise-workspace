package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avcheck/internal/clock"
)

func TestRenderDeterministic(t *testing.T) {
	spec := Spec{Width: 160, Height: 90, FPS: 10, Duration: clock.FromSamples(48000, 48000)}
	stereo := make([]int16, 48000*2)
	for i := range stereo {
		stereo[i] = int16((i * 37) % 20000)
	}
	events := []Event{{Tick: clock.FromMillis(500), Label: "beep"}}

	dir1, dir2 := t.TempDir(), t.TempDir()
	f1, err := Render(dir1, spec, stereo, events)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Render(dir2, spec, stereo, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(f1) != 10 {
		t.Fatalf("frames %d", len(f1))
	}
	for i := range f1 {
		if f1[i].SHA256 != f2[i].SHA256 {
			t.Fatalf("frame %d not deterministic", i)
		}
		if len(f1[i].SHA256) != 64 {
			t.Fatalf("bad digest %q", f1[i].SHA256)
		}
	}
	// Manifest exists and lists every frame.
	man, err := os.ReadFile(filepath.Join(dir1, "frames.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(man)), "\n"); got != 9 {
		t.Fatalf("manifest lines %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir1, "frame-0003.png")); err != nil {
		t.Fatal("frame file missing")
	}
	// Distinct frames must have distinct digests (timecode strip differs).
	if f1[0].SHA256 == f1[1].SHA256 {
		t.Fatal("frames 0 and 1 should differ")
	}
}
