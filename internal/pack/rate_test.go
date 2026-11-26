package pack

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

// 平滑渐变纹理帧（贴近真实素材，JPEG 可压缩）
func testFrames(n, w, h int) []image.Image {
	frames := make([]image.Image, n)
	for i := range frames {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetNRGBA(x, y, color.NRGBA{
					uint8((x*4 + i*7) % 256),
					uint8((y*4 + i*3) % 256),
					uint8((x + y + i) % 256),
					255,
				})
			}
		}
		frames[i] = img
	}
	return frames
}

func TestRateControlApproachesTarget(t *testing.T) {
	frames := testFrames(10, 64, 64)
	// 先探出中等质量的实际码率作为目标
	probe, err := RateControl(frames, 2000, 400)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RateControl(frames, probe.ActualKbps, 400)
	if err != nil {
		t.Fatal(err)
	}
	if res.DeltaPct > 8 || res.DeltaPct < -8 {
		t.Fatalf("delta = %v%%, want within ±8%%", res.DeltaPct)
	}
	if res.PSNRMeanDB < 20 {
		t.Fatalf("psnr mean = %v dB, too low", res.PSNRMeanDB)
	}
	if res.PSNRMinDB > res.PSNRMeanDB {
		t.Fatalf("min %v > mean %v", res.PSNRMinDB, res.PSNRMeanDB)
	}
	if len(res.Frames) != len(frames) {
		t.Fatalf("frames = %d", len(res.Frames))
	}
}

func TestRateControlDeterministic(t *testing.T) {
	frames := testFrames(6, 64, 64)
	r1, err := RateControl(frames, 1500, 240)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := RateControl(frames, 1500, 240)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Quality != r2.Quality || r1.ActualKbps != r2.ActualKbps {
		t.Fatalf("nondeterministic: q %d/%d kbps %v/%v", r1.Quality, r2.Quality, r1.ActualKbps, r2.ActualKbps)
	}
	for i := range r1.Frames {
		if !bytes.Equal(r1.Frames[i], r2.Frames[i]) {
			t.Fatalf("frame %d bytes differ", i)
		}
	}
}

func TestRateControlErrors(t *testing.T) {
	frames := testFrames(2, 8, 8)
	if _, err := RateControl(nil, 100, 100); err == nil {
		t.Fatal("no frames accepted")
	}
	if _, err := RateControl(frames, 0, 100); err == nil {
		t.Fatal("zero target accepted")
	}
	if _, err := RateControl(frames, 100, 0); err == nil {
		t.Fatal("zero duration accepted")
	}
}
