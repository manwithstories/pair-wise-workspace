package render

import (
	"image"
	"os"
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
)

func TestDecode16Bit(t *testing.T) {
	// 2 声道 2 帧：帧0=(10,-20)，帧1=(30,-40)
	pcm := []byte{
		10, 0, 0xEC, 0xFF, // 10, -20
		30, 0, 0xD8, 0xFF, // 30, -40
	}
	tracks, frames, err := Decode16Bit(pcm, 2)
	if err != nil || frames != 2 {
		t.Fatalf("Decode: frames=%d err=%v", frames, err)
	}
	if tracks[0][0] != 10 || tracks[1][0] != -20 || tracks[1][1] != -40 {
		t.Fatalf("声道拆分错误: %+v", tracks)
	}
	if _, _, err := Decode16Bit([]byte{1, 2, 3}, 2); clock.CodeOf(err) != clock.CodeInvalidParam {
		t.Fatalf("奇数长度应拒绝, 得 %v", err)
	}
}

func TestWaveformDeterministic(t *testing.T) {
	tracks := [][]int16{
		make([]int16, 4800),
		make([]int16, 4800),
	}
	for i := range tracks[0] {
		tracks[0][i] = int16(i % 1000)
		tracks[1][i] = -tracks[0][i]
	}
	a := WaveformImage(tracks, 0, 4800, 0)
	b := WaveformImage(tracks, 0, 4800, 0)
	if len(a.Pix) != FrameWidth*FrameHeight || &a.Pix[0] == &b.Pix[0] {
		t.Fatal("位图尺寸或独立性异常")
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			t.Fatal("同输入波形位图不确定")
		}
	}
}

func TestSearchQualityMonotonic(t *testing.T) {
	img := imageGrayFixture()
	// 质量越高，JPEG 字节总体越大：比较两端
	lo, bLo, _, _, err := SearchQuality(img, 1)
	if err != nil {
		t.Fatal(err)
	}
	hi, bHi, _, _, err := SearchQuality(img, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if lo < 1 || hi > 100 || len(bLo) >= len(bHi) {
		t.Fatalf("质量档单调性错误: qLo=%d(%dB) qHi=%d(%dB)", lo, len(bLo), hi, len(bHi))
	}
	// 目标字节给定时，结果应落在 1..100 且非空
	q, b, _, dec, err := SearchQuality(img, 8000)
	if err != nil || q < 1 || q > 100 || len(b) == 0 || dec == nil {
		t.Fatalf("质量搜索异常: q=%d len=%d err=%v", q, len(b), err)
	}
}

func TestRenderFrameToDisk(t *testing.T) {
	dir := t.TempDir()
	img := imageGrayFixture()
	q, b, _, dec, err := SearchQuality(img, 4000)
	if err != nil {
		t.Fatal(err)
	}
	fm, err := RenderFrame(dir, 7, img, dec, b)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "frame_000007.jpg")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("帧文件未生成: %v", err)
	}
	if fm.File != "frame_000007.jpg" || len(fm.SHA256) != 64 || fm.PSNR <= 0 || q == 0 {
		t.Fatalf("帧指标异常: %+v", fm)
	}
}

func TestPathRejected(t *testing.T) {
	if _, err := clockSafeJoin(t.TempDir(), "../escape.jpg"); clock.CodeOf(err) != clock.CodePathRejected {
		t.Fatalf("越界路径应拒绝, 得 %v", err)
	}
}

func imageGrayFixture() *image.Gray {
	tracks := [][]int16{make([]int16, 960), make([]int16, 960), make([]int16, 960)}
	for i := range tracks[0] {
		v := int16((i*37) % 20000)
		tracks[0][i], tracks[1][i], tracks[2][i] = v, -v, v/2
	}
	return WaveformImage(tracks, 0, 960, 0)
}
