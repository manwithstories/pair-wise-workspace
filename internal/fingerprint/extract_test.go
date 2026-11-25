package fingerprint

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func ffmpegOK() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// renderVideo writes a deterministic test pattern clip. Two clips rendered
// from the same lavfi source and time range contain identical frames.
func renderVideo(t *testing.T, dir, name string) string {
	t.Helper()
	if !ffmpegOK() {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=15:duration=3",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-an", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render failed: %v\n%s", err, b)
	}
	return out
}

func TestNormalizeVideoStable(t *testing.T) {
	dir := t.TempDir()
	path := renderVideo(t, dir, "v.mp4")
	geo := DefaultGeometry
	tr := TimeRange{StartMillis: 0, DurationMs: 1000}
	f1, err := NormalizeVideo(context.Background(), path, 0, tr, geo)
	if err != nil {
		t.Fatal(err)
	}
	if len(f1) != geo.FramesPerWindow {
		t.Fatalf("frames = %d, want %d", len(f1), geo.FramesPerWindow)
	}
	for i, f := range f1 {
		if len(f) != geo.Width*geo.Height {
			t.Fatalf("frame %d size %d wrong", i, len(f))
		}
	}
	// Rerun must produce byte-identical normalized frames.
	f2, err := NormalizeVideo(context.Background(), path, 0, tr, geo)
	if err != nil {
		t.Fatal(err)
	}
	for i := range f1 {
		if string(f1[i]) != string(f2[i]) {
			t.Fatalf("frame %d not stable across reruns", i)
		}
	}
}

func TestNormalizedVideoFingerprintDedup(t *testing.T) {
	dir := t.TempDir()
	p1 := renderVideo(t, dir, "a.mp4")
	p2 := renderVideo(t, dir, "b.mp4")
	geo := DefaultGeometry
	tr := TimeRange{StartMillis: 500, DurationMs: 1000}
	frames1, err := NormalizeVideo(context.Background(), p1, 0, tr, geo)
	if err != nil {
		t.Fatal(err)
	}
	frames2, err := NormalizeVideo(context.Background(), p2, 0, tr, geo)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, 480)
	w1 := CanonicalWindow{SegmentID: "a", AudioRate: 48000, MonoPCM: pcm,
		Geometry: geo, GrayFrames: frames1}
	w2 := CanonicalWindow{SegmentID: "b", AudioRate: 48000, MonoPCM: pcm,
		Geometry: geo, GrayFrames: frames2}
	fp1, _ := Fingerprint(w1)
	fp2, _ := Fingerprint(w2)
	if fp1 != fp2 {
		t.Fatalf("identical content produced different fingerprints:\n%s\n%s", fp1, fp2)
	}
	d := NewDeduper()
	if d1, _ := d.Add(w1); d1.Duplicate {
		t.Fatalf("first window cannot be a duplicate: %+v", d1)
	}
	d2, _ := d.Add(w2)
	if !d2.Duplicate || d2.CanonicalID != "a" {
		t.Fatalf("expected dedup hit referencing first window, got %+v", d2)
	}
	if d.Hits() != 1 {
		t.Fatalf("hits = %d, want 1", d.Hits())
	}
}

func TestNormalizeVideoInvalidRange(t *testing.T) {
	if _, err := NormalizeVideo(context.Background(), "x", 0,
		TimeRange{StartMillis: 0, DurationMs: 0}, DefaultGeometry); err == nil {
		t.Fatal("zero duration accepted")
	}
}
