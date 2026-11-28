package audit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"presspack/internal/core"
)

func renderAndEncode(t *testing.T) (src, playlist, variantDir string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	src = filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=4",
		"-c:v", "libx264", "-g", "25", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	variantDir = filepath.Join(dir, "v400")
	if err := os.MkdirAll(variantDir, 0o755); err != nil {
		t.Fatal(err)
	}
	playlist = filepath.Join(variantDir, "index.m3u8")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-i", src,
		"-c:v", "libx264", "-b:v", "400000", "-maxrate", "400000", "-bufsize", "800000",
		"-g", "50", "-sc_threshold", "0", "-pix_fmt", "yuv420p",
		"-f", "hls", "-hls_time", "2", "-hls_segment_type", "fmp4",
		"-hls_playlist_type", "vod", "-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(variantDir, "seg_%05d.m4s"),
		playlist).CombinedOutput(); err != nil {
		t.Fatalf("encode: %v\n%s", err, out)
	}
	return src, playlist, variantDir
}

func TestMeasureBitrateError(t *testing.T) {
	_, _, vdir := renderAndEncode(t)
	pct, err := MeasureBitrateError(map[string]string{"v400": vdir},
		map[string]int{"v400": 400000}, core.FromMillis(4000))
	if err != nil {
		t.Fatal(err)
	}
	if pct > MaxBitrateErrorPct {
		t.Errorf("bitrate error %.2f%% exceeds %.0f%%", pct, MaxBitrateErrorPct)
	}
}

func TestMeasureSSIMAndDecodeErrors(t *testing.T) {
	src, playlist, _ := renderAndEncode(t)
	ssim, err := MeasureSSIM(context.Background(), src, playlist)
	if err != nil {
		t.Fatal(err)
	}
	if ssim < MinSSIMAvg {
		t.Errorf("ssim = %.3f below %.2f", ssim, MinSSIMAvg)
	}
	n, err := CountDecodeErrors(context.Background(), playlist)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("decode errors = %d", n)
	}
}

func TestHashTreeDeterministic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.bin"), []byte("aaa"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b.bin"), []byte("bbb"), 0o644)
	h1, err := HashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := HashTree(dir)
	if h1 != h2 {
		t.Error("same tree must hash equally")
	}
	os.WriteFile(filepath.Join(dir, "sub", "b.bin"), []byte("bbc"), 0o644)
	h3, _ := HashTree(dir)
	if h3 == h1 {
		t.Error("changed content must change hash")
	}
}

func TestMetricsPassAndPrint(t *testing.T) {
	good := Metrics{Cameras: 3, Variants: 2, SegmentsPerVariant: 6,
		BitrateErrorPct: 10, SSIMAvg: 0.95, MaxAlignMs: 20, RepeatHash: "match"}
	if !good.Pass() {
		t.Error("good metrics must pass")
	}
	bad := good
	bad.DecodeErrors = 1
	if bad.Pass() {
		t.Error("decode errors must fail")
	}
	bad = good
	bad.RepeatHash = "mismatch"
	if bad.Pass() {
		t.Error("hash mismatch must fail")
	}
	var sb strings.Builder
	good.Print(&sb)
	for _, k := range []string{"cameras=3", "variants=2", "segments_per_variant=6",
		"bitrate_error_pct=", "ssim_avg=", "decode_errors=0",
		"recovered_spans=", "max_align_ms=20", "repeat_hash=match"} {
		if !strings.Contains(sb.String(), k) {
			t.Errorf("print missing %q:\n%s", k, sb.String())
		}
	}
}
