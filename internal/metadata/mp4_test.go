package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediad/internal/clock"
)

func parseDemo(t *testing.T, corrupt bool) *Asset {
	t.Helper()
	dir := t.TempDir()
	path, err := WriteDemoMP4(dir, corrupt)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := ParseMP4(path)
	if corrupt {
		// Track-local failures are isolated and reported per track; ParseMP4
		// only returns a top-level error when no track survived.
		if asset == nil || len(asset.Failures) == 0 {
			t.Fatalf("expected isolated track failures, got asset=%v err=%v", asset, err)
		}
		return asset
	}
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestParseMP4NormalizesDemo(t *testing.T) {
	asset := parseDemo(t, false)
	if len(asset.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(asset.Tracks))
	}
	v0, ok := asset.Track("v0")
	if !ok {
		t.Fatal("missing v0")
	}
	if v0.Rotation != 90 || v0.OutputWidth() != 1080 || v0.OutputHeight() != 1920 {
		t.Fatalf("v0 rotation/dims: rot=%d out=%dx%d", v0.Rotation, v0.OutputWidth(), v0.OutputHeight())
	}
	if v0.Color.Primaries != PrimariesBT2020 || !v0.Color.FullRange || v0.Color.Source != SourceColr {
		t.Fatalf("v0 color: %+v", v0.Color)
	}
	if v0.Timebase != clock.MustNew(1, 30000) {
		t.Fatalf("v0 timebase %s", v0.Timebase)
	}
	if v0.EditStart != 900 {
		t.Fatalf("v0 edit start %d", v0.EditStart)
	}
	if v0.Duration != 60000 {
		t.Fatalf("v0 duration %d", v0.Duration)
	}
	v1, _ := asset.Track("v1")
	if v1.Crop == nil || v1.Crop.W != 1024 || v1.Crop.H != 576 {
		t.Fatalf("v1 crop %+v", v1.Crop)
	}
	if v1.Color.Primaries != PrimariesBT709 {
		t.Fatalf("v1 color %+v", v1.Color)
	}
	a0, ok := asset.Track("a0")
	if !ok || a0.Kind != KindAudio || a0.SampleRate != 48000 || a0.Channels != 2 {
		t.Fatalf("audio track %+v", a0)
	}
	if a0.Timebase != clock.MustNew(1, 48000) || a0.Duration != 96000 {
		t.Fatalf("audio timing %+v", a0)
	}
	if len(asset.Failures) != 0 {
		t.Fatalf("unexpected failures %+v", asset.Failures)
	}
}

func TestParseMP4Rotations(t *testing.T) {
	cases := []struct {
		in   float64
		want int
	}{{0, 0}, {90, 90}, {180, 180}, {270, 270}, {44, 0}, {46, 90}, {300, 270}}
	for _, c := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, "r.mp4")
		spec := MP4Spec{Tracks: []MP4TrackSpec{{Kind: KindVideo, ID: 1, Timescale: 90000, Duration: 9000, Width: 640, Height: 360, RotationDegrees: c.in}}}
		if err := WriteMP4(path, spec); err != nil {
			t.Fatal(err)
		}
		asset, err := ParseMP4(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := asset.Tracks[0].Rotation; got != c.want {
			t.Fatalf("rotation %v -> %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseMP4CorruptIsolatesTracks(t *testing.T) {
	asset := parseDemo(t, true)
	if len(asset.Failures) < 2 {
		t.Fatalf("failures = %d, want crop and color failures", len(asset.Failures))
	}
	var sawCrop, sawColor bool
	for _, f := range asset.Failures {
		if f.Key == "v0" && strings.Contains(f.Err.Error(), "interval=") && strings.Contains(f.Err.Error(), "track=v0") {
			sawCrop = true
		}
		if f.Key == "v1" && f.Err.Category == "metadata" {
			sawColor = true
		}
		if f.Err.BlockingDep == "" {
			t.Fatalf("failure without blocking dep: %v", f.Err)
		}
	}
	if !sawCrop || !sawColor {
		t.Fatalf("crop=%t color=%t failures=%+v", sawCrop, sawColor, asset.Failures)
	}
	// The audio branch must survive isolated corruption.
	if _, ok := asset.Track("a0"); !ok {
		t.Fatal("audio track lost despite isolation")
	}
}

func TestParseMP4Truncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.mp4")
	spec := demoMP4Spec(false)
	spec.TruncateTo = 40
	if err := WriteMP4(path, spec); err != nil {
		t.Fatal(err)
	}
	_, err := ParseMP4(path)
	if err == nil {
		t.Fatal("expected an error for truncated container")
	}
	if got := err.(interface{ Error() string }).Error(); !strings.Contains(got, "metadata") {
		t.Fatalf("error not categorized: %v", err)
	}
}

func TestParseMP4MissingFile(t *testing.T) {
	if _, err := ParseMP4(filepath.Join(t.TempDir(), "absent.mp4")); err == nil {
		t.Fatal("expected read error")
	}
	if _, err := os.Stat("absent.mp4"); err == nil {
		t.Skip("unexpected file")
	}
}
