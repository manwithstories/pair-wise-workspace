package dsp

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestDecodeL16Mono(t *testing.T) {
	raw := make([]byte, 4)
	binary.BigEndian.PutUint16(raw[0:2], uint16(int16(32767)))
	binary.BigEndian.PutUint16(raw[2:4], 0)
	b, err := DecodeL16(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.Frames() != 2 || math.Abs(b.Interleaved[0]-32767.0/32768.0) > 1e-6 {
		t.Fatalf("bad decode: %+v", b.Interleaved)
	}
}

func TestNormalizeStereoDuplicatesMono(t *testing.T) {
	in := &Buffer{Rate: 44100, Channels: 1, Interleaved: []float64{0.25, -0.5}}
	out := NormalizeStereo(in)
	if out.Channels != 2 || out.Frames() != 2 {
		t.Fatalf("bad shape channels=%d frames=%d", out.Channels, out.Frames())
	}
	want := []float64{0.25, 0.25, -0.5, -0.5}
	for i := range want {
		if out.Interleaved[i] != want[i] {
			t.Fatalf("sample %d=%v want %v", i, out.Interleaved[i], want[i])
		}
	}
}

func TestResample441To48FrameCount(t *testing.T) {
	const frames = 88200 // 2 seconds, and 600*147
	in := &Buffer{Rate: 44100, Channels: 1, Interleaved: make([]float64, frames)}
	for i := range in.Interleaved {
		in.Interleaved[i] = math.Sin(2 * math.Pi * 1000 * float64(i) / 44100)
	}
	out := Resample(in, 48000)
	if out.Rate != 48000 || out.Frames() != 96000 {
		t.Fatalf("rate=%d frames=%d, want 48000/96000", out.Rate, out.Frames())
	}
	// A 1 kHz tone should retain appreciable energy near the middle.
	var energy float64
	for _, v := range out.Interleaved[48000:49000] {
		energy += v * v
	}
	if energy/1000 < 0.1 {
		t.Fatalf("resampled tone energy too low: %f", energy/1000)
	}
}

func TestResample48PassthroughPreservesSamples(t *testing.T) {
	in := &Buffer{Rate: 48000, Channels: 2, Interleaved: []float64{0.1, 0.2, -0.1, -0.2}}
	out := Resample(in, 48000)
	if &out.Interleaved[0] == &in.Interleaved[0] {
		t.Fatal("Resample should return a copy for same-rate input")
	}
	for i := range in.Interleaved {
		if out.Interleaved[i] != in.Interleaved[i] {
			t.Fatalf("sample %d changed", i)
		}
	}
}

func TestEncodeS16LEIsLittleEndian(t *testing.T) {
	b := &Buffer{Channels: 1, Interleaved: []float64{1}}
	raw := EncodeS16LE(b)
	if raw[0] != 0xff || raw[1] != 0x7f {
		t.Fatalf("max sample encoding=%02x %02x", raw[0], raw[1])
	}
}
