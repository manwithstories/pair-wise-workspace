package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mediadelivery/internal/probe"
)

func TestEncodeSegmentSmoke(t *testing.T) {
	spec := SegmentSpec{
		Width: 320, Height: 180, FrameRate: 30,
		SampleRate: 48000, Channels: 2,
		DurationSec:  2,
		SegmentIndex: 0,
		ColorTag:     ColorBT709,
	}
	data, err := EncodeSegment(spec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(data) < 1000 {
		t.Fatalf("segment too small: %d bytes", len(data))
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "seg0.ts")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := probe.Probe(p)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(rep.Streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(rep.Streams))
	}
	var v, a int
	for _, s := range rep.Streams {
		switch s.MediaType {
		case "video":
			v++
			if s.Width != 320 || s.Height != 180 {
				t.Fatalf("video size = %dx%d", s.Width, s.Height)
			}
		case "audio":
			a++
			if s.SampleRate != 48000 || s.Channels != 2 {
				t.Fatalf("audio = %dHz %dch", s.SampleRate, s.Channels)
			}
		}
	}
	if v != 1 || a != 1 {
		t.Fatalf("video=%d audio=%d", v, a)
	}
	t.Logf("encoded %d bytes, duration90k=%d, color=%+v", len(data), rep.Duration90k, rep.Streams[0].Color)
}
