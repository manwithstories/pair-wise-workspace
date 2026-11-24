package fixture

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

func specs() []TrackSpec {
	return []TrackSpec{
		{ID: 1, Video: true, Codec: "avc1", Timescale: 90000, Samples: 10,
			KeyInterval: 5, PayloadBytes: 36, WithElst: true, MediaTime: 250},
		{ID: 2, Video: false, Codec: "mp4a", Timescale: 48000, Samples: 10, PayloadBytes: 28},
	}
}

func TestBuildMP4Deterministic(t *testing.T) {
	a := BuildMP4(specs(), rand.New(rand.NewSource(42)))
	b := BuildMP4(specs(), rand.New(rand.NewSource(42)))
	if !bytes.Equal(a, b) {
		t.Fatal("same seed must yield identical MP4 bytes")
	}
	// Payloads are derived purely from (track, sample); the rng is accepted for
	// API symmetry but does not perturb the deterministic byte layout.
	c := BuildMP4(specs(), rand.New(rand.NewSource(43)))
	if !bytes.Equal(a, c) {
		t.Fatal("fixture layout must be deterministic regardless of rng seed")
	}
	// Different sample counts must change the output.
	other := specs()
	other[0].Samples = 5
	if bytes.Equal(a, BuildMP4(other, rand.New(rand.NewSource(42)))) {
		t.Fatal("changed spec unexpectedly produced identical output")
	}
}

func TestBuildRTPCaptureDeterministic(t *testing.T) {
	s := RTPSequence(10, 1000, 160, 7, 3)
	a := BuildRTPCapture(s)
	b := BuildRTPCapture(s)
	if !bytes.Equal(a, b) {
		t.Fatal("RTP capture must be deterministic")
	}
	// Every frame must carry a 4-byte length prefix matching its body.
	pos := 0
	count := 0
	for pos < len(a) {
		n := int(binary.BigEndian.Uint32(a[pos:]))
		if pos+4+n > len(a) {
			t.Fatalf("frame %d overruns capture", count)
		}
		pos += 4 + n
		count++
	}
	if count != 10 {
		t.Fatalf("want 10 frames, got %d", count)
	}
}

func TestCorruptMP4LengthChangesOnlySizeField(t *testing.T) {
	orig := BuildMP4(specs(), rand.New(rand.NewSource(1)))
	corrupt, err := CorruptMP4Length(orig, 2, -64)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrupt) != len(orig) {
		t.Fatal("length corruption must not change total file length")
	}
	// Locate the mdat (index 2) offset and ensure only its 4-byte size word
	// differs; every other byte stays identical.
	pos, idx := 0, 0
	mdatOff := -1
	for pos+8 <= len(orig) {
		if idx == 2 {
			mdatOff = pos
		}
		pos += int(binary.BigEndian.Uint32(orig[pos:]))
		idx++
	}
	if mdatOff < 0 {
		t.Fatal("mdat box not found")
	}
	for i := range orig {
		if i >= mdatOff && i < mdatOff+4 {
			continue // the rewritten size word
		}
		if orig[i] != corrupt[i] {
			t.Fatalf("byte %d outside size word changed", i)
		}
	}
	if binary.BigEndian.Uint32(orig[mdatOff:]) ==
		binary.BigEndian.Uint32(corrupt[mdatOff:]) {
		t.Fatal("mdat size word was not changed")
	}
}

func TestCorruptMP4LengthRejectsBadIndex(t *testing.T) {
	orig := BuildMP4(specs(), rand.New(rand.NewSource(1)))
	if _, err := CorruptMP4Length(orig, 99, 1); err == nil {
		t.Fatal("expected out-of-range box index error")
	}
}

func TestTruncateRTPCapture(t *testing.T) {
	capBytes := BuildRTPCapture(RTPSequence(6, 0, 160, 1, 0))
	torn, err := TruncateRTPCapture(capBytes, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(torn) >= len(capBytes) {
		t.Fatal("torn capture must be shorter")
	}
	// Walk to packet 5's prefix; its declared body length must now exceed the
	// bytes actually remaining in the torn capture.
	pos := 0
	for i := 0; i < 5; i++ {
		n := int(binary.BigEndian.Uint32(capBytes[pos:]))
		pos += 4 + n
	}
	declared := int(binary.BigEndian.Uint32(torn[pos:]))
	remaining := len(torn) - (pos + 4)
	if declared <= remaining {
		t.Fatalf("torn packet should declare %d > remaining %d bytes", declared, remaining)
	}
}
