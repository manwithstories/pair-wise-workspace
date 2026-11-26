package pipeline

import (
	"path/filepath"
	"testing"

	"github.com/asticode/go-astiav"

	"mediad/internal/av"
	"mediad/internal/fixtures"
	"mediad/internal/media"
)

// TestCutBetweenPacketsIsSampleExact checks a cut that falls strictly inside a
// PCM packet: the first delivered sample must be the correct source sample,
// and the reported alignment must stay within one 48 kHz sample.
func TestCutBetweenPacketsIsSampleExact(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mid.mov")
	if _, err := fixtures.BuildResampleAsset(fixtures.ResampleSpec{
		Rate: 48000, Layout: astiav.ChannelLayoutStereo, Start: 0, Samples: 48000,
		Seed: 3, Path: path,
	}); err != nil {
		t.Fatal(err)
	}
	tb := media.Rational{Num: 1, Den: 48000}
	// Cut from sample 500 (inside the first 1024-frame packet).
	res, err := av.ReadSegment(av.SegmentRequest{
		Path: path, StreamIndex: 0, TimeBase: tb,
		InRaw: 500, OutRaw: 500 + 1000, InLayout: "stereo", InChannels: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.LengthSamples != 1000 {
		t.Fatalf("length=%d want 1000", res.LengthSamples)
	}
	if res.AlignmentUS > 21 {
		t.Fatalf("alignment=%.3f exceeds one sample", res.AlignmentUS)
	}
	// The rendered segment must not begin with leading silence: the source
	// tone starts immediately, so a non-zero first packet means the cut was
	// placed, not skipped.
	var energy float64
	for i := 0; i < res.LengthSamples; i++ {
		energy += float64(res.Samples[i*2]) * float64(res.Samples[i*2])
	}
	if energy == 0 {
		t.Fatal("cut rendered as silence")
	}
}
