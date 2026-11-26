package ffmpeg

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// requireTools skips the test when ffmpeg is not installed (CI without the
// Homebrew dependency). On the target macOS host it always runs.
func requireTools(t *testing.T) (context.Context, string) {
	t.Helper()
	if _, _, err := Binaries(); err != nil {
		t.Skipf("ffmpeg not available: %v", err)
	}
	return context.Background(), t.TempDir()
}

func TestProbeMissingFile(t *testing.T) {
	if _, _, err := Binaries(); err != nil {
		t.Skip("ffmpeg not available")
	}
	ctx := context.Background()
	if _, err := Probe(ctx, filepath.Join(t.TempDir(), "nope.mp4")); err == nil {
		t.Fatal("expected probe error on missing file")
	}
}

func TestGenerateAndProbeVFRVideo(t *testing.T) {
	ctx, dir := requireTools(t)
	out := filepath.Join(dir, "vfr.mp4")
	concat := filepath.Join(dir, "list.txt")
	durations := []float64{0.5, 1.0, 1.5, 0.5, 1.0, 1.5, 0.5, 1.0}
	spec := VideoSpec{
		OutPath: out, Width: 64, Height: 36, FrameCount: len(durations),
		FrameDurations: durations, KeyframeEvery: 4, StartPTS: 1.0,
	}
	done := make(chan error, 1)
	go func() { done <- GenerateVFRMP4(ctx, spec, concat) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("generate vfr: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("ffmpeg generation timed out")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output missing: %v", err)
	}
	streams, err := Probe(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	var video *StreamInfo
	for i := range streams {
		if streams[i].CodecType == "video" {
			video = &streams[i]
		}
	}
	if video == nil {
		t.Fatal("no video stream probed")
	}
	if video.CodecName != "h264" || video.Width != 64 || video.Height != 36 {
		t.Fatalf("unexpected video: %+v", video)
	}
	if video.NbFrames <= 0 {
		t.Fatalf("frame count not probed: %d", video.NbFrames)
	}
}

func TestGenerateAudioAndRemuxAndExtract(t *testing.T) {
	ctx, dir := requireTools(t)
	audio := filepath.Join(dir, "a.m4a")
	if err := GenerateAudioMP4(ctx, audio, 3, 48000); err != nil {
		t.Fatal(err)
	}
	streams, err := Probe(ctx, audio)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].CodecName != "aac" {
		t.Fatalf("unexpected audio probe: %+v", streams)
	}
	// Remux a sub-range by stream copy.
	clip := filepath.Join(dir, "clip.mp4")
	if err := Remux(ctx, RemuxOptions{InPath: audio, OutPath: clip,
		StartSeconds: 0.5, EndSeconds: 2.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clip); err != nil {
		t.Fatalf("remux output missing: %v", err)
	}
	// Missing source path surfaces the typed sentinel.
	if err := Remux(ctx, RemuxOptions{OutPath: filepath.Join(dir, "x.mp4"),
		Missing: true}); err != ErrMissingSource {
		t.Fatalf("want ErrMissingSource, got %v", err)
	}
}

func TestExtractFrameWritesPNG(t *testing.T) {
	ctx, dir := requireTools(t)
	out := filepath.Join(dir, "v.mp4")
	concat := filepath.Join(dir, "list.txt")
	spec := VideoSpec{OutPath: out, Width: 32, Height: 32, FrameCount: 6,
		FrameDurations: []float64{1, 1, 1, 1, 1, 1}, KeyframeEvery: 2}
	if err := GenerateVFRMP4(ctx, spec, concat); err != nil {
		t.Fatal(err)
	}
	png := filepath.Join(dir, "frame.png")
	if err := ExtractFrame(ctx, out, png, 2.0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(png)
	if err != nil || info.Size() == 0 {
		t.Fatalf("png not written: %v", err)
	}
}

func TestParseFraction(t *testing.T) {
	n, d := parseFraction("90000/1")
	if n != 90000 || d != 1 {
		t.Fatalf("fraction parse wrong: %d/%d", n, d)
	}
	if n, d = parseFraction("garbage"); n != 0 || d != 1 {
		t.Fatalf("invalid fraction fallback wrong: %d/%d", n, d)
	}
}
