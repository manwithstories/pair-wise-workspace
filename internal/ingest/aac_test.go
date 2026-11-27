package ingest

import (
	"encoding/binary"
	"testing"

	"mediacheck/internal/model"
)

func TestAACFixtureGeneratesADTSFrames(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 25 // one second
	cfg.Codec = model.CodecAAC
	g, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Assets.AudioExtra) == 0 {
		t.Fatal("AAC fixture has no AudioSpecificConfig")
	}
	if len(g.Audio.Packets) == 0 {
		t.Fatal("no audio packets")
	}
	for i, p := range g.Audio.Packets {
		d := p.Data
		if len(d) < 7 || d[0] != 0xFF || d[1]&0xF0 != 0xF0 {
			t.Fatalf("packet %d is not ADTS: %x", i, d[:min(8, len(d))])
		}
		if p.Codec != model.CodecAAC {
			t.Fatalf("packet %d codec = %v", i, p.Codec)
		}
		// The ADTS header must declare exactly the bytes that follow it.
		frameLen := int(d[3]&0x03)<<11 | int(d[4])<<3 | int(d[5]>>5)
		if frameLen != len(d) {
			t.Fatalf("packet %d declares %d bytes, carries %d", i, frameLen, len(d))
		}
		if p.Duration != 1024 {
			t.Fatalf("packet %d duration = %d, want 1024 AAC samples", i, p.Duration)
		}
	}
	if binary.BigEndian.Uint16(g.Assets.AudioExtra[:2])&0xF0 == 0 && len(g.Assets.AudioExtra) >= 2 {
		t.Fatalf("AudioSpecificConfig looks empty: %x", g.Assets.AudioExtra)
	}
}

func TestAACFixtureIsDeterministic(t *testing.T) {
	cfg := DefaultConfig(7)
	cfg.VideoFrames = 5
	cfg.Codec = model.CodecAAC
	a, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Audio.Packets) != len(b.Audio.Packets) {
		t.Fatal("packet counts differ")
	}
	for i := range a.Audio.Packets {
		if string(a.Audio.Packets[i].Data) != string(b.Audio.Packets[i].Data) {
			t.Fatalf("AAC packet %d differs between runs", i)
		}
	}
}

func TestConfigRejectsNonAudioFixtureCodec(t *testing.T) {
	cfg := DefaultConfig(1)
	cfg.VideoFrames = 1
	cfg.Codec = model.CodecH264
	if _, err := Generate(cfg); err == nil {
		t.Fatal("expected error for video fixture codec")
	}
}
