package rewrap_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"bcast/internal/rewrap"
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
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}

func TestCopyCompatibleKeyframeRange(t *testing.T) {
	dir := t.TempDir()
	root, err := safe.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	srcRel := "assets/a.mp4"
	p, err := root.Resolve(srcRel)
	if err != nil {
		t.Fatal(err)
	}
	makeClip(t, p)

	res, err := rewrap.CopyCompatible(context.Background(), root, srcRel, "out/copy.mp4", 2, 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA256 == "" || res.Samples == 0 {
		t.Fatalf("bad result: %+v", res)
	}
	if err := root.VerifyHash(res.PathRel, res.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeRawRGBCommitsValidMP4(t *testing.T) {
	dir := t.TempDir()
	root, err := safe.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan []byte, 2)
	for i := 0; i < 2; i++ {
		f := make([]byte, 160*120*3)
		for j := range f {
			f[j] = byte(i*80 + j%32)
		}
		frames <- f
	}
	close(frames)
	res, err := rewrap.EncodeRawRGB(context.Background(), root, "out/composite.mp4", 160, 120, 8, frames)
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 2 {
		t.Fatalf("samples=%d want 2", res.Samples)
	}
	if err := root.VerifyHash(res.PathRel, res.SHA256); err != nil {
		t.Fatal(err)
	}
}
