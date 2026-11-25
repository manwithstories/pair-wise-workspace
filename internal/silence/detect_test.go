package silence

import (
	"encoding/binary"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/codec"
)

func pcm(amp int16, n int) []byte {
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(amp))
	}
	return b
}

func unit(start, dur int64, payload []byte) codec.AccessUnit {
	return codec.AccessUnit{
		Start:   clock.At(start, clock.Base48k),
		Dur:     clock.At(dur, clock.Base48k),
		Payload: payload,
	}
}

func TestRMS(t *testing.T) {
	if r := RMS(nil, 0); r != 0 {
		t.Fatalf("empty RMS=%f, want 0", r)
	}
	if r := RMS(pcm(16384, 100), 0); r < 0.49 || r > 0.51 {
		t.Fatalf("RMS=%f, want ~0.5", r)
	}
	// A 1-byte codec header must not skew the samples.
	withHeader := append([]byte{0xAB}, pcm(16384, 100)...)
	if r := RMS(withHeader, 1); r < 0.49 || r > 0.51 {
		t.Fatalf("RMS with header=%f, want ~0.5", r)
	}
}

func TestDetectMergesAdjacent(t *testing.T) {
	units := []codec.AccessUnit{
		unit(0, 960, pcm(100, 480)),      // silent
		unit(960, 960, pcm(100, 480)),    // silent, merges
		unit(1920, 960, pcm(20000, 480)), // loud
		unit(2880, 960, pcm(50, 480)),    // silent again
		unit(3840, 0, nil),               // zero-duration: ignored
	}
	got := Detect("studio", units, 0.01, 0)
	if len(got) != 2 {
		t.Fatalf("intervals=%d, want 2: %+v", len(got), got)
	}
	if got[0].Start.Ticks != 0 || got[0].End.Ticks != 1920 {
		t.Fatalf("first interval %+v, want 0..1920", got[0])
	}
	if got[1].Start.Ticks != 2880 || got[1].End.Ticks != 3840 {
		t.Fatalf("second interval %+v, want 2880..3840", got[1])
	}
	if got[0].Track != "studio" {
		t.Fatalf("track lost: %+v", got[0])
	}
}
