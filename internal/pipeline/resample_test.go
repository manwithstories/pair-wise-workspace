package pipeline

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/asticode/go-astiav"

	"mediad/internal/av"
	"mediad/internal/fixtures"
	"mediad/internal/media"
)

// TestResample44k1To48k checks a 44.1 kHz stereo asset resamples to the 48 kHz
// delivery bus with the expected sample count and a balanced image.
func TestResample44k1To48k(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "resample.mov")
	if _, err := fixtures.BuildResampleAsset(fixtures.ResampleSpec{
		Rate: 44100, Layout: astiav.ChannelLayoutStereo, Start: 44100, Samples: 44100 * 3,
		Seed: 7, Path: path,
	}); err != nil {
		t.Fatal(err)
	}
	tb := media.Rational{Num: 1, Den: 44100}
	res, err := av.ReadSegment(av.SegmentRequest{
		Path: path, StreamIndex: 0, TimeBase: tb,
		InRaw: 44100, OutRaw: 44100 * 2, InLayout: "stereo", InChannels: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := res.LengthSamples - 48000; d < -1 || d > 1 {
		t.Fatalf("length=%d want ~48000", res.LengthSamples)
	}
	var eL, eR float64
	for i := 0; i < res.LengthSamples; i++ {
		eL += float64(res.Samples[i*2]) * float64(res.Samples[i*2])
		eR += float64(res.Samples[i*2+1]) * float64(res.Samples[i*2+1])
	}
	if eL == 0 || eR == 0 {
		t.Fatalf("empty channels L=%.3f R=%.3f", eL, eR)
	}
	if math.Abs(eL-eR) > 0.05*math.Max(eL, eR) {
		t.Fatalf("left/right mismatch L=%.3f R=%.3f", eL, eR)
	}
}
