package fixture

import (
	"crypto/sha256"
	"testing"
)

func TestEncodeDeterministicAndSurround(t *testing.T) {
	base := SegmentSpec{Width: 160, Height: 90, FrameRate: 25, SampleRate: 48000,
		Channels: 2, DurationSec: 1, SegmentIndex: 3}
	a, _ := EncodeSegment(base)
	b, _ := EncodeSegment(base)
	if sha256.Sum256(a) != sha256.Sum256(b) {
		t.Fatal("segment encoding is not deterministic")
	}
	sur := base
	sur.Channels = 6
	sur.SampleRate = 44100
	d, err := EncodeSegment(sur)
	if err != nil {
		t.Fatalf("5.1/44.1k encode: %v", err)
	}
	if len(d) == 0 {
		t.Fatal("empty surround segment")
	}
	// Different segment index -> different content -> different hash.
	other := base
	other.SegmentIndex = 4
	c, _ := EncodeSegment(other)
	if sha256.Sum256(a) == sha256.Sum256(c) {
		t.Fatal("distinct segments produced identical bytes")
	}
}
