package render_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"bcast/internal/model"
	"bcast/internal/render"
	"bcast/internal/safe"
)

func makeClip(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i",
		"testsrc=size=160x120:rate=8:duration=1",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-g", "2", "-bf", "0",
		"-force_key_frames", "expr:eq(n,0)+eq(n,2)+eq(n,4)+eq(n,6)",
		"-fflags", "+bitexact", "-flags:v", "+bitexact", "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}

func TestExportRangeHasStableFrameHashes(t *testing.T) {
	dir := t.TempDir()
	root, err := safe.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	mp4Rel := "assets/a.mp4"
	mp4Path, err := root.Resolve(mp4Rel)
	if err != nil {
		t.Fatal(err)
	}
	makeClip(t, mp4Path)

	ctx := context.Background()
	frames1, err := render.ExportRange(ctx, root, mp4Rel, "frames/first", "cut", "a", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	frames2, err := render.ExportRange(ctx, root, mp4Rel, "frames/second", "cut", "a", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames1) != 2 || len(frames2) != 2 {
		t.Fatalf("frame counts %d/%d", len(frames1), len(frames2))
	}
	for i := range frames1 {
		if frames1[i].SHA256 != frames2[i].SHA256 {
			t.Fatalf("frame %d hash differs across identical exports", i)
		}
		if frames1[i].SourceFrame != int64(i) {
			t.Fatalf("source frame=%d want %d", frames1[i].SourceFrame, i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "frames", "first", "cut-000.ppm")); err != nil {
		t.Fatalf("review frame missing: %v", err)
	}
}

func TestDissolveUsesExactAlpha(t *testing.T) {
	a := make([]byte, render.FrameBytes)
	b := make([]byte, render.FrameBytes)
	for i := range b {
		b[i] = 200
	}
	alpha := model.Rational{N: 1, D: 4}
	out, err := render.Dissolve(a, b, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != 50 {
		t.Fatalf("blend=%d want 50", out[0])
	}
}
