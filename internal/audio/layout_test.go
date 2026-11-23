package audio

import (
	"testing"

	"mediad/internal/clock"
)

func TestValidateClean(t *testing.T) {
	n, issues := Validate(Track{
		LayoutName: "5.1", SampleRate: 48000,
		Channels:       []string{"FL", "FR", "FC", "LFE", "SL", "SR"},
		PrimingSamples: 512,
	})
	if len(issues) != 0 {
		t.Fatalf("issues=%v", issues)
	}
	if len(n.Channels) != 6 || len(n.Unmapped) != 0 {
		t.Fatalf("normalized=%+v", n)
	}
	want := int64(512) * clock.Timescale / 48000
	if n.OffsetTicks != want {
		t.Fatalf("offset=%d, want %d", n.OffsetTicks, want)
	}
}

func TestValidateUnmappableChannel(t *testing.T) {
	n, issues := Validate(Track{
		LayoutName: "stereo", SampleRate: 48000,
		Channels: []string{"FL", "FR", "WIBBLE"},
	})
	if len(n.Unmapped) != 1 || n.Unmapped[0] != "WIBBLE" {
		t.Fatalf("unmapped=%v", n.Unmapped)
	}
	if len(n.Channels) != 2 {
		t.Fatalf("channels=%v", n.Channels)
	}
	found := false
	for _, i := range issues {
		if i.Kind == "channel" {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues=%v", issues)
	}
}

func TestValidateBadRateAndLayout(t *testing.T) {
	_, issues := Validate(Track{LayoutName: "quad", SampleRate: 12345, Channels: []string{"A"}})
	var layout, rate bool
	for _, i := range issues {
		layout = layout || i.Kind == "layout"
		rate = rate || i.Kind == "rate"
	}
	if !layout || !rate {
		t.Fatalf("issues=%v", issues)
	}
}

func TestMapRate(t *testing.T) {
	if n, d, exact := MapRate(48000, 48000); n != 1 || d != 1 || !exact {
		t.Fatalf("identity: %d/%d exact=%v", n, d, exact)
	}
	if n, d, exact := MapRate(44100, 48000); n != 160 || d != 147 || exact {
		t.Fatalf("44.1->48: %d/%d exact=%v", n, d, exact)
	}
	if _, _, ok := MapRate(0, 48000); ok {
		t.Fatal("zero rate must not map")
	}
}
