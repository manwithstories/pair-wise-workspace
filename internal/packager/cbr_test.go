package packager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCBRPadding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cbr.mp4")
	f, _ := os.Create(path)
	defer f.Close()
	// 2-bit 打包帧：960 采样 * 2ch / 4 = 480 字节/帧；50 帧 = 1s
	p, err := New(f, 48000, 2, 960, 480, 256)
	if err != nil {
		t.Fatal(err)
	}
	frame := bytes.Repeat([]byte{0x1b}, 480)
	for i := 0; i < 50; i++ {
		if err := p.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	res, err := p.Close()
	if err != nil {
		t.Fatal(err)
	}
	// 目标 256kbps、1 秒 → 32000 字节；实际码率偏差 < 1%
	if res.ActualKbps < 253.4 || res.ActualKbps > 258.6 {
		t.Fatalf("actual=%f kbps, bytes=%d", res.ActualKbps, res.Bytes)
	}
	// 无填充时（target=0）码率仅由媒体决定
	path2 := filepath.Join(t.TempDir(), "raw.mp4")
	f2, _ := os.Create(path2)
	defer f2.Close()
	p2, _ := New(f2, 48000, 2, 960, 480, 0)
	for i := 0; i < 50; i++ {
		p2.WriteFrame(frame)
	}
	res2, err := p2.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res2.ActualKbps >= 256 {
		t.Fatalf("unpadded should be below target: %f", res2.ActualKbps)
	}
	if res2.Bytes >= res.Bytes {
		t.Fatalf("padding did not grow file: %d vs %d", res2.Bytes, res.Bytes)
	}
}
