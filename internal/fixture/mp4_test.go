package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/index"
)

func TestBuildMP4Indexable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cam0.mp4")
	if err := BuildMP4(path, 100, 30_000, 1000); err != nil {
		t.Fatalf("build mp4: %v", err)
	}
	idx, err := index.BuildFile(path)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	// 100 samples with a 50-sample GOP: samples 1 and 51.
	if len(idx.Keyframes) != 2 {
		t.Fatalf("keyframes = %d, want 2", len(idx.Keyframes))
	}
	if idx.Timescale != 30_000 {
		t.Fatalf("timescale = %d", idx.Timescale)
	}
	// The second indexed sample is sample 51: 50000 ticks at 30kHz.
	if idx.Keyframes[1].TimeNS != 1_666_666_666 {
		t.Fatalf("second keyframe time = %d", idx.Keyframes[1].TimeNS)
	}
	for _, kf := range idx.Keyframes {
		if !kf.Verified {
			t.Fatalf("unverified keyframe: %+v", kf)
		}
	}
	if idx.Roundtrip != 1.0 {
		t.Fatalf("roundtrip = %f", idx.Roundtrip)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	cfg := DefaultGenConfig(42)
	cfg.Faults = FaultSet{Reorder: true, Drift: true}
	a := Generate(cfg)
	b := Generate(cfg)
	if len(a.Packets) != len(b.Packets) {
		t.Fatalf("packet counts differ")
	}
	for i := range a.Packets {
		if a.Packets[i] != b.Packets[i] {
			t.Fatalf("packet %d differs", i)
		}
	}
}

func TestParseFaults(t *testing.T) {
	f := ParseFaults("reorder,truncate,drift")
	if !f.Reorder || !f.Truncate || !f.Drift || f.Drop {
		t.Fatalf("parsed = %+v", f)
	}
}

func TestBuildMP4FileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.mp4")
	if err := BuildMP4(path, 20, 25_000, 1000); err != nil {
		t.Fatalf("build: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() < 100 {
		t.Fatalf("stat: %v size=%d", err, st.Size())
	}
}
