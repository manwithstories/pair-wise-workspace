package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// genTrack 生成一轨确定性测试帧，返回清单帧记录行。
func genTrack(t *testing.T, root, track string, n int, pts func(i int) int64) []string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, track), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 0; i < n; i++ {
		img := image.NewNRGBA(image.Rect(0, 0, 160, 120))
		for y := 0; y < 120; y++ {
			for x := 0; x < 160; x++ {
				img.SetNRGBA(x, y, color.NRGBA{
					uint8(x*2 + i + (x*y)%17),
					uint8(y*2 + i*2 + (x*x+y)%13),
					uint8(x + y + (y*y)%11),
					255,
				})
			}
		}
		rel := fmt.Sprintf("%s/%04d.jpg", track, i)
		var sb strings.Builder
		if err := jpeg.Encode(&sb, img, &jpeg.Options{Quality: 92}); err != nil {
			t.Fatal(err)
		}
		data := []byte(sb.String())
		if err := os.WriteFile(filepath.Join(root, rel), data, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		lines = append(lines, fmt.Sprintf(
			`{"type":"frame","track":%q,"seq":%d,"pts":%d,"dur":3600,"sha256":%q,"file":%q}`,
			track, i, pts(i), hex.EncodeToString(sum[:]), rel))
	}
	return lines
}

func writeTestManifest(t *testing.T, root string, tracks string, recs ...[]string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"header","version":1,"timebase":{"num":1,"den":90000},"tracks":[%s]}`, tracks)
	b.WriteByte('\n')
	for _, lines := range recs {
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	p := filepath.Join(root, "manifest.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const twoTracks = `{"id":"camA","priority":1,"fps":25,"modulus":0},{"id":"camB","priority":2,"fps":25,"modulus":0}`

func setupCase(t *testing.T) (srcRoot, mPath string) {
	t.Helper()
	srcRoot = t.TempDir()
	pts := func(i int) int64 { return int64(i) * 3600 }
	a := genTrack(t, srcRoot, "camA", 12, pts)
	b := genTrack(t, srcRoot, "camB", 12, pts)
	mPath = writeTestManifest(t, srcRoot, twoTracks, a, b)
	return srcRoot, mPath
}

func runOpts(src, mPath, out string) Options {
	return Options{ManifestPath: mPath, SourceRoot: src, OutDir: out,
		TargetKbps: 900, Width: 160, Height: 120}
}

func TestRunNormal(t *testing.T) {
	src, mPath := setupCase(t)
	res, err := Run(context.Background(), runOpts(src, mPath, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Frames != 12 || res.GapFrames != 0 || res.BadFrames != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.AlignMaxMs > 1 {
		t.Fatalf("align = %v ms", res.AlignMaxMs)
	}
	if res.Rate.DeltaPct > 8 || res.Rate.DeltaPct < -8 {
		t.Fatalf("rate delta = %v%%", res.Rate.DeltaPct)
	}
	if res.Rate.PSNRMeanDB < 28 {
		t.Fatalf("psnr mean = %v", res.Rate.PSNRMeanDB)
	}
	if len(res.OutputSHA) != 64 {
		t.Fatalf("output sha = %q", res.OutputSHA)
	}
}

func TestRunStrictBadHash(t *testing.T) {
	src, mPath := setupCase(t)
	// 破坏一帧内容
	f := filepath.Join(src, "camA", "0003.jpg")
	data, _ := os.ReadFile(f)
	data[0] ^= 0xff
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := runOpts(src, mPath, t.TempDir())
	opts.Strict = true
	_, err := Run(context.Background(), opts)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("strict bad hash err = %v, want exit code 2", err)
	}
	// 非严格模式：坏帧隔离并记录影响范围
	opts.Strict = false
	opts.OutDir = t.TempDir()
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.BadFrames != 1 || len(res.BadRanges) == 0 {
		t.Fatalf("bad isolation = %+v", res)
	}
}

func TestRunResumeMatchesFull(t *testing.T) {
	src, mPath := setupCase(t)
	full, err := Run(context.Background(), runOpts(src, mPath, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	// 截断中断
	outDir := t.TempDir()
	opts := runOpts(src, mPath, outDir)
	opts.InterruptAfter = 5
	if _, err := Run(context.Background(), opts); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt err = %v", err)
	}
	// 续跑
	opts.InterruptAfter = 0
	opts.Resume = true
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed || res.OutputSHA != full.OutputSHA {
		t.Fatalf("resume hash %q != full %q", res.OutputSHA, full.OutputSHA)
	}
	// 参数摘要不匹配拒绝续跑
	opts.TargetKbps = 500
	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("param mismatch resume accepted")
	}
}
