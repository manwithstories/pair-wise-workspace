package metadata

import (
	"bytes"
	"testing"
)

// TestEscapeRoundTrip locks the emulation-prevention codec: decoding an
// escaped RBSP must reproduce the exact original bytes, including runs of
// zeros that would otherwise look like start codes.
func TestEscapeRoundTrip(t *testing.T) {
	cases := [][]byte{
		{0, 0, 0, 1},
		{0, 0, 0, 0, 0, 0, 1},
		{0x67, 0x10, 0, 0, 0, 7, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 3, 0, 0, 3, 0, 0, 3},
	}
	for _, in := range cases {
		got := nalEmulationPrevention(escapeRBSP(in))
		if !bytes.Equal(got, in) {
			t.Fatalf("EP round trip mismatch\n in=% x\nout=% x", in, got)
		}
	}
}

// TestSplitNALsOffsets verifies that a 4-byte then 3-byte start code pair
// yields NAL units with correct header offsets.
func TestSplitNALsOffsets(t *testing.T) {
	stream := []byte{
		0, 0, 0, 1, 0x06, 0xAA, // first NAL (4-byte start)
		0, 0, 1, 0x06, 0xBB, // second NAL (3-byte start)
	}
	units, starts := splitNALs(stream)
	if len(units) != 2 || len(starts) != 2 {
		t.Fatalf("got %d units, want 2", len(units))
	}
	if starts[0] != 0 || starts[1] != 6 {
		t.Fatalf("start offsets = %v, want [0 6]", starts)
	}
	// headerOff points just past the start code.
	if units[0].headerOff != 4 || units[1].headerOff != 9 {
		t.Fatalf("header offsets = %d,%d, want 4,9", units[0].headerOff, units[1].headerOff)
	}
}
