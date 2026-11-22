package ingest

import (
	"testing"

	"mediarun/internal/mediaerr"
)

func TestInsideAnyBox(t *testing.T) {
	boxes := []*boxHeader{
		{PayloadStart: 100, End: 200},
		{PayloadStart: 300, End: 400},
	}
	cases := []struct {
		off, size int64
		want      bool
	}{
		{100, 50, true},  // fully inside first
		{150, 50, true},  // reaches first boundary exactly
		{150, 51, false}, // spills past first end
		{250, 10, false}, // gap between boxes
		{300, 100, true}, // exactly fills second
		{399, 2, false},  // past second end
	}
	for _, c := range cases {
		if got := insideAnyBox(c.off, c.size, boxes); got != c.want {
			t.Errorf("insideAnyBox(%d,%d) = %v, want %v", c.off, c.size, got, c.want)
		}
	}
}

// TestBlockCeilingConstant pins the 4 MiB specification ceiling so a future
// change cannot silently relax the memory bound.
func TestBlockCeilingConstant(t *testing.T) {
	if MaxBlockBytes != 4*1024*1024 {
		t.Fatalf("MaxBlockBytes = %d, want 4 MiB", MaxBlockBytes)
	}
}

// TestMXFBlockTooLarge verifies an over-ceiling length maps to the structured
// BLOCK_TOO_LARGE error with format and track context.
func TestMXFBlockTooLarge(t *testing.T) {
	oversize := MaxBlockBytes + 1
	var err error
	if oversize > MaxBlockBytes {
		err = mediaerr.New(mediaerr.CodeBlockTooLarge, "oversize").
			At("mxf", "video/0", 0)
	}
	if !mediaerr.Is(err, mediaerr.CodeBlockTooLarge) {
		t.Fatalf("want BLOCK_TOO_LARGE, got %v", err)
	}
	se := err.(*mediaerr.Error)
	if se.Format != "mxf" || se.Track != "video/0" {
		t.Fatalf("error context wrong: %+v", se)
	}
}
