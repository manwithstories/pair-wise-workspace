package mkv

import (
	"errors"
	"testing"
)

// Only the frame-targeted error and the size arithmetic need extra coverage
// beyond block_test.go: malformed EBML lacing deltas and fixed-lacing
// divisibility failures.

func TestFixedLacingIndivisible(t *testing.T) {
	payload := append(header(1, 0, LacingFixed<<1), 1, 'a', 'b', 'c')
	_, err := ParseSimpleBlock(payload, 0)
	if !errors.Is(err, ErrLacing) {
		t.Fatalf("got %v", err)
	}
}

func TestEBMLLacingOversizedDelta(t *testing.T) {
	// Two frames (count-1 = 1): first size 1, remainder 1. The implied last
	// frame is fine, so a larger declared first size must be rejected.
	payload := append(header(1, 0, LacingEBML<<1), 1, 0x90, 'a')
	_, err := ParseSimpleBlock(payload, 0)
	if !errors.Is(err, ErrLacing) {
		t.Fatalf("got %v", err)
	}
}

func TestXiphLacingMissingContinuation(t *testing.T) {
	// Frame count 2 (three frames) but only one size byte and no data.
	payload := append(header(1, 0, LacingXiph<<1), 2, 0xFF)
	_, err := ParseSimpleBlock(payload, 0)
	if !errors.Is(err, ErrLacing) {
		t.Fatalf("got %v", err)
	}
}

func TestLacedFrameOffsetsAreContiguous(t *testing.T) {
	lace := append([]byte{1}, []byte("abcd")...)
	payload := append(header(1, 0, LacingFixed<<1), lace...)
	b, err := ParseSimpleBlock(payload, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Frames) != 2 {
		t.Fatalf("frames=%d", len(b.Frames))
	}
	first, second := b.Frames[0], b.Frames[1]
	if second.SourceOffset != first.SourceOffset+int64(first.Size) {
		t.Fatalf("frames not contiguous: %d then %d", first.SourceOffset, second.SourceOffset)
	}
	if b.SourceOffset != 100 || first.SourceOffset != 100+4+1 {
		t.Fatalf("offsets: block=%d frame=%d", b.SourceOffset, first.SourceOffset)
	}
}
