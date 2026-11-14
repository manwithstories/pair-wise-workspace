package codec

import (
	"math"
	"math/rand"
	"testing"

	"tcr/internal/yuv"
)

func TestRoundTrip(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	f, _ := yuv.NewFrame(64, 48)
	for i := range f.Y {
		f.Y[i] = byte(100 + rnd.Intn(60))
	}
	for i := range f.Cb {
		f.Cb[i] = 128
		f.Cr[i] = 128
	}
	hdr := SegmentHeader{SegIndex: 0, FrameCount: 1, W: 64, H: 48, FPS: 25, TargetKbps: 100}
	path := t.TempDir() + "/seg_00.tcr"
	if _, err := WriteSegmentFile(path, hdr, []*yuv.Frame{f}, []int{4}); err != nil {
		t.Fatal(err)
	}
	seg, err := ReadSegmentFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var se float64
	for i := range f.Y {
		d := float64(f.Y[i]) - float64(seg.Frames[0].Y[i])
		se += d * d
	}
	mse := se / float64(len(f.Y))
	psnr := 10 * math.Log10(255*255/mse)
	t.Logf("PSNR=%.2f dB", psnr)
	if psnr < 40 {
		t.Fatalf("round-trip PSNR too low: %.2f", psnr)
	}
	if seg.Header.TargetKbps != 100 || seg.QPs[0] != 4 {
		t.Fatal("header/QP mismatch")
	}
}
