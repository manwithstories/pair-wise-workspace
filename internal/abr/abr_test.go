package abr

import (
	"math"
	"testing"

	"avpipe/internal/media"
)

func TestControllerSelectsTierFromBandwidth(t *testing.T) {
	c := NewController(4)
	// 200 kbps available: only the 180k tier fits the 15% headroom.
	for i := 0; i < 4; i++ {
		c.Observe(200_000)
	}
	if got := c.Decide(); got != 180 {
		t.Fatalf("Decide at 200kbps = %d, want 180", got)
	}

	// 600 kbps: 450k fits (needs 517.5k), 900k does not.
	c2 := NewController(4)
	for i := 0; i < 4; i++ {
		c2.Observe(600_000)
	}
	if got := c2.Decide(); got != 450 {
		t.Fatalf("Decide at 600kbps = %d, want 450", got)
	}

	// 1.2 Mbps: the top tier fits.
	c3 := NewController(4)
	for i := 0; i < 4; i++ {
		c3.Observe(1_200_000)
	}
	if got := c3.Decide(); got != 900 {
		t.Fatalf("Decide at 1.2Mbps = %d, want 900", got)
	}
}

func TestEstimateIsConservativeLowerHalf(t *testing.T) {
	c := NewController(4)
	for _, bps := range []int64{1_000_000, 200_000, 300_000, 900_000} {
		c.Observe(bps)
	}
	// Lower half rounded up keeps the two smallest samples: 200k and 300k.
	if got, want := c.Estimate(), int64(250_000); got != want {
		t.Fatalf("Estimate = %d, want %d", got, want)
	}
}

func TestBitrateToleranceBoundary(t *testing.T) {
	cases := []struct {
		actual int64
		ok     bool
	}{
		{180_000, true},
		{216_000, true},  // exactly +20%
		{144_000, true},  // exactly -20%
		{217_000, false}, // just over
		{143_000, false}, // just under
	}
	for _, tc := range cases {
		got := WithinTolerance(180, tc.actual)
		if got != tc.ok {
			t.Errorf("WithinTolerance(180, %d) = %v, want %v", tc.actual, got, tc.ok)
		}
	}
}

func TestPSNRIdenticalAndNoisy(t *testing.T) {
	w, h := 16, 16
	src := &media.Frame{Width: w, Height: h, Y: make([]byte, w*h),
		Cb: make([]byte, 64), Cr: make([]byte, 64)}
	for i := range src.Y {
		src.Y[i] = byte(i % 256)
	}
	if got := FramePSNR(src, src); !math.IsInf(got, 1) {
		t.Errorf("identical frames PSNR = %v, want +Inf", got)
	}
	noisy := &media.Frame{Width: w, Height: h, Y: append([]byte(nil), src.Y...),
		Cb: append([]byte(nil), src.Cb...), Cr: append([]byte(nil), src.Cr...)}
	noisy.Y[0] += 10
	got := FramePSNR(src, noisy)
	if got <= 28 || math.IsInf(got, 1) {
		t.Errorf("single +10 luma error PSNR = %v, want finite >28dB", got)
	}
}

func TestDownscaleI420Dimensions(t *testing.T) {
	src := &media.Frame{Width: 8, Height: 8,
		Y:  make([]byte, 64),
		Cb: make([]byte, 16), Cr: make([]byte, 16)}
	dst := DownscaleI420(src, 4, 4)
	if dst.Width != 4 || dst.Height != 4 || len(dst.Y) != 16 ||
		len(dst.Cb) != 4 || len(dst.Cr) != 4 {
		t.Fatalf("downscale geometry wrong: %dx%d y=%d cb=%d cr=%d",
			dst.Width, dst.Height, len(dst.Y), len(dst.Cb), len(dst.Cr))
	}
}

func TestAVDriftMS(t *testing.T) {
	// Audio 900 ticks (10 ms) behind video on the 90 kHz clock.
	if got := AVDriftMS(0, 900); math.Abs(got-10.0) > 1e-9 {
		t.Fatalf("AVDriftMS = %v, want 10", got)
	}
	// Audio ahead: negative drift.
	if got := AVDriftMS(1800, 900); got >= 0 {
		t.Fatalf("AVDriftMS = %v, want negative", got)
	}
}
