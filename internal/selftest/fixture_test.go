package selftest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/mp4"
)

func ffmpegInstalled() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func TestGenerateFixtures(t *testing.T) {
	if !ffmpegInstalled() {
		t.Skip("ffmpeg not installed")
	}
	opts := Options{Dir: t.TempDir(), Seed: 73}
	fx, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{fx.Stereo48MP4, fx.Stereo441MP4, fx.SurroundMOV,
		fx.VideoOnlyMP4, fx.TruncatedPath, fx.BadLenPath, fx.BoundaryPath}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.Size() == 0 {
			t.Fatalf("fixture missing or empty: %s (%v)", p, err)
		}
	}
}

func TestGeneratedGoodInputsParse(t *testing.T) {
	if !ffmpegInstalled() {
		t.Skip("ffmpeg not installed")
	}
	fx, err := Generate(Options{Dir: t.TempDir(), Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path       string
		wantTracks int
		audioCh    int
		audioRate  uint32
	}{
		{fx.Stereo48MP4, 2, 2, 48000},
		{fx.Stereo441MP4, 2, 2, 44100},
		{fx.SurroundMOV, 2, 6, 44100},
	}
	for _, c := range cases {
		f, err := os.Open(c.path)
		if err != nil {
			t.Fatal(err)
		}
		container, err := mp4.Parse(f, mp4.DefaultLimits)
		f.Close()
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(c.path), err)
		}
		if len(container.Tracks) != c.wantTracks {
			t.Fatalf("%s tracks=%d want %d", c.path, len(container.Tracks), c.wantTracks)
		}
		for _, tr := range container.Tracks {
			if tr.Kind == mp4.KindAudio {
				if tr.Channels != uint16(c.audioCh) || tr.SampleRate != c.audioRate {
					t.Fatalf("%s audio ch=%d rate=%d want %d/%d",
						c.path, tr.Channels, tr.SampleRate, c.audioCh, c.audioRate)
				}
			}
			if tr.Kind == mp4.KindVideo && tr.Color == nil {
				t.Fatalf("%s video lost color metadata", c.path)
			}
		}
	}
}

func TestGeneratedCorruptionDetected(t *testing.T) {
	if !ffmpegInstalled() {
		t.Skip("ffmpeg not installed")
	}
	fx, err := Generate(Options{Dir: t.TempDir(), Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
	}{
		{fx.TruncatedPath}, {fx.BadLenPath},
	} {
		f, err := os.Open(c.path)
		if err != nil {
			t.Fatal(err)
		}
		_, perr := mp4.Parse(f, mp4.DefaultLimits)
		f.Close()
		var pe *clock.Error
		if perr == nil {
			t.Fatalf("corrupt fixture %s parsed cleanly", filepath.Base(c.path))
		}
		if !errors.As(perr, &pe) || pe.Kind != clock.KindCorrupt || pe.Offset < 0 {
			t.Fatalf("%s want corrupt with byte offset, got %v", c.path, perr)
		}
	}
}

func TestBoundaryFixtureValid(t *testing.T) {
	if !ffmpegInstalled() {
		t.Skip("ffmpeg not installed")
	}
	fx, _ := Generate(Options{Dir: t.TempDir(), Seed: 3})
	f, err := os.Open(fx.BoundaryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := mp4.Parse(f, mp4.DefaultLimits)
	if err != nil {
		t.Fatalf("boundary fixture should be valid: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("boundary fixture tracks=%d want 2", len(c.Tracks))
	}
}
