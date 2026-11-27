package fixture

import (
	"crypto/sha256"
	"testing"
)

// TestSilenceAndColorVariants exercises fixture features the analysis stage
// relies on: real silent audio and distinct colour-metadata tags.
func TestSilenceAndColorVariants(t *testing.T) {
	base := SegmentSpec{Width: 160, Height: 90, FrameRate: 25, SampleRate: 48000,
		Channels: 2, DurationSec: 1, SegmentIndex: 1}

	audible, err := EncodeSegment(base)
	if err != nil {
		t.Fatal(err)
	}
	s0 := base
	s0.SilentAudio = true
	silentBytes, err := EncodeSegment(s0)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(silentBytes) == sha256.Sum256(audible) {
		t.Fatal("silent and audible segments are byte-identical")
	}

	bt709 := base
	bt709.ColorTag = ColorBT709
	bt2020 := base
	bt2020.ColorTag = ColorBT2020
	c709, err := EncodeSegment(bt709)
	if err != nil {
		t.Fatal(err)
	}
	c2020, err := EncodeSegment(bt2020)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(c709) == sha256.Sum256(c2020) {
		t.Fatal("bt709 and bt2020 colour tags produced identical segments")
	}
}
