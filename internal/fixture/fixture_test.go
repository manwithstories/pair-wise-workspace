package fixture

import (
	"os"
	"path/filepath"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

func healthySpec() MP4Spec {
	return MP4Spec{
		Name: "healthy",
		Tracks: []TrackSpec{
			{Kind: "video", Timescale: 24_000, SampleCount: 120, SampleDelta: 1000,
				Width: 640, Height: 360, IncludeColr: true, Primaries: 1, Transfer: 1,
				Matrix: 1, IncludeSyncTable: true, KeyFrameEvery: 12},
			{Kind: "audio", Timescale: 48_000, SampleCount: 240, SampleDelta: 1024,
				Channels: 2, SampleRate: 48_000},
			{Kind: "markers", Timescale: 1000, SampleCount: 60, SampleDelta: 500},
		},
	}
}

func TestWriteMP4RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "healthy.mp4")
	if err := WriteMP4(path, healthySpec()); err != nil {
		t.Fatalf("WriteMP4: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var trakCount, stssCount, co64Count int
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.Path[len(h.Path)-1] {
		case mp4.BoxTypeTrak():
			trakCount++
		case mp4.BoxTypeStss():
			stssCount++
		case mp4.BoxTypeCo64():
			co64Count++
		}
		return h.Expand()
	}
	if _, err := mp4.ReadBoxStructure(f, handler); err != nil {
		t.Fatalf("re-parse generated mp4: %v", err)
	}
	if trakCount != 3 {
		t.Errorf("trak count = %d, want 3", trakCount)
	}
	if stssCount != 1 {
		t.Errorf("stss count = %d, want 1", stssCount)
	}
	if co64Count != 3 {
		t.Errorf("co64 count = %d, want 3", co64Count)
	}
}

func TestWriteMP4Truncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cut.mp4")
	spec := healthySpec()
	spec.Name = "cut"
	spec.TruncateAt = 120
	if err := WriteMP4(path, spec); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 120 {
		t.Errorf("truncated size = %d, want 120", info.Size())
	}
}
