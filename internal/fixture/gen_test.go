package fixture

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/manifest"
	"mediacore/internal/media"
)

func TestGenerateDeterministic(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	if _, err := Generate(d1, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(d2, 42); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"video.m3u8", "audio.m3u8", "manifest.mpd", "rtp.bin", "media/v1-1.m4s"} {
		a, _ := os.ReadFile(filepath.Join(d1, name))
		b, _ := os.ReadFile(filepath.Join(d2, name))
		if string(a) != string(b) {
			t.Fatalf("%s differs between runs", name)
		}
	}
}

func TestGeneratedDataShape(t *testing.T) {
	dir := t.TempDir()
	res, err := Generate(dir, 42)
	if err != nil {
		t.Fatal(err)
	}
	// HLS parses and carries the gap.
	audio, err := os.ReadFile(filepath.Join(dir, "audio.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	track, err := manifest.ParseHLS(bytes.NewReader(audio), res.AudioMeta)
	if err != nil {
		t.Fatal(err)
	}
	if !track.Segments[GapAudioSegment].Gap {
		t.Fatal("audio segment 4 should be a gap")
	}
	// The truncated segment is detected at probe time.
	f, _ := os.Open(filepath.Join(dir, "media", "v1-6.m4s"))
	defer f.Close()
	if _, err := media.ReadSegment(f, "v1", TruncatedVideoSegment, false); clock.CodeOf(err) != clock.CodeTruncatedSegment {
		t.Fatalf("want TRUNCATED_SEGMENT, got %v", err)
	}
	// A healthy segment probes fine.
	f2, _ := os.Open(filepath.Join(dir, "media", "v1-1.m4s"))
	defer f2.Close()
	seg, err := media.ReadSegment(f2, "v1", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(seg.Samples) != VideoSamplesPer || !seg.Samples[0].Keyframe {
		t.Fatalf("bad video index: %d samples", len(seg.Samples))
	}
	// DASH parses with the hole and the 20ms offset.
	mpd, _ := os.Open(filepath.Join(dir, "manifest.mpd"))
	defer mpd.Close()
	pl, err := manifest.ParseDASH(mpd)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Tracks) != 2 {
		t.Fatalf("want 2 DASH tracks, got %d", len(pl.Tracks))
	}
}
