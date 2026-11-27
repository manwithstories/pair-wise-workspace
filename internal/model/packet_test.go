package model

import (
	"testing"

	"mediacheck/internal/clock"
)

func TestCodecParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Codec
	}{
		{"opus", CodecOpus},
		{"AAC", CodecAAC},
		{"h264", CodecH264},
		{"avc", CodecH264},
	} {
		got, err := ParseCodec(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ParseCodec(%q) = %v, %v", tc.in, got, err)
		}
	}
	if _, err := ParseCodec("vp9"); err == nil {
		t.Fatal("expected error for unsupported codec")
	}
	if !CodecOpus.IsAudio() || !CodecAAC.IsAudio() || CodecH264.IsAudio() {
		t.Fatal("IsAudio classification wrong")
	}
	if CodecUnknown.String() != "unknown" {
		t.Fatal("unexpected codec name")
	}
}

func TestPacketSpanAndClone(t *testing.T) {
	p := Packet{
		Track:    TrackAudio,
		PTS:      960,
		Duration: 960,
		Data:     []byte{1, 2, 3},
		Timebase: clock.Audio(48000),
		Slot:     Slot{Start: 0, End: 20_000_000},
	}
	if got := p.End(); got != 1920 {
		t.Fatalf("End = %d, want 1920", got)
	}
	c := p.Clone()
	c.Data[0] = 9
	if p.Data[0] != 1 {
		t.Fatal("Clone aliased payload")
	}
	if c.PTS != p.PTS || c.Slot != p.Slot {
		t.Fatal("Clone lost metadata")
	}
}

func TestFixtureHelpers(t *testing.T) {
	audio := StreamInfo{Track: TrackAudio, Codec: CodecOpus}
	f := Fixture{
		Streams: []StreamInfo{audio},
		Packets: []Packet{{Track: TrackAudio, Data: make([]byte, 10)}, {Track: TrackAudio, Data: make([]byte, 5)}},
	}
	if got := f.PacketBytes(); got != 15 {
		t.Fatalf("PacketBytes = %d, want 15", got)
	}
	if _, ok := f.TrackInfo(TrackAudio); !ok {
		t.Fatal("audio stream not found")
	}
	if _, ok := f.TrackInfo(TrackVideo); ok {
		t.Fatal("video stream unexpectedly found")
	}
}
