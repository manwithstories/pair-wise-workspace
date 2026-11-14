package slicer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"m3u8ctl/internal/ts"
)

func TestGenerateAlignedSegments(t *testing.T) {
	root := t.TempDir()
	segments, err := Generate(Config{
		OutputDir: root, Segments: 3, TargetDuration: 2 * time.Second, FrameRate: 30,
		Renditions: []Rendition{{Name: "1080p", Resolution: "1920x1080", Bitrate: 480000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 3 {
		t.Fatalf("generated %d segments, want 3", len(segments))
	}
	for i := 0; i < 3; i++ {
		path := filepath.Join(root, "1080p", filepath.Base(segments[i].Path))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		probe, err := ts.ProbeFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size()%ts.PacketSize != 0 {
			t.Fatalf("%s size %d is not TS aligned", path, info.Size())
		}
		if !probe.HasPCR || len(probe.Keyframes) == 0 {
			t.Fatalf("%s missing PCR or keyframe: %+v", path, probe)
		}
		if diff := probe.Duration() - 2*time.Second; diff < -time.Millisecond || diff > time.Millisecond {
			t.Fatalf("%s duration = %s", path, probe.Duration())
		}
	}
}
