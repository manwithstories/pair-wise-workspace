package mp4

import (
	"testing"

	"mediad/internal/metadata"
)

func TestInitMetadataRoundTrip(t *testing.T) {
	spec := InitSpec{
		Width: 3840, Height: 2160, VideoTimescale: 90000,
		Color: &metadata.Color{ColorType: "nclx", Primaries: 9, Transfer: 16, Matrix: 9, FullRange: true},
		AudioTimescale: 48000,
		Audio: &metadata.Audio{
			Channels: 6, SampleRateHz: 48000, SampleRateRaw: 48000 << 16,
			LayoutCode: 6, Layout: "L R C LFE Ls Rs",
		},
	}
	b, err := EncodeInit(spec)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ParseInit(b)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	v, ok := info.Video()
	if !ok {
		t.Fatal("no video track")
	}
	if v.Timescale != 90000 || v.Width != 3840 || v.Height != 2160 {
		t.Fatalf("video basics wrong: %+v", v)
	}
	if v.Color == nil || *v.Color != *spec.Color {
		t.Fatalf("color raw not preserved: %+v", v.Color)
	}
	a, ok := info.Audio()
	if !ok || a.Audio == nil {
		t.Fatal("no audio track")
	}
	if a.Timescale != 48000 {
		t.Fatalf("audio timescale=%d", a.Timescale)
	}
	got := a.Audio
	if got.Channels != 6 || got.SampleRateHz != 48000 || got.Layout != spec.Audio.Layout || got.LayoutCode != 6 {
		t.Fatalf("audio raw not preserved: %+v", got)
	}
}

func TestInitRejectsTruncated(t *testing.T) {
	spec := InitSpec{Color: &metadata.Color{Primaries: 1}}
	b, err := EncodeInit(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInit(b[:len(b)-40]); err == nil {
		t.Fatal("expected error on truncated init")
	}
}
