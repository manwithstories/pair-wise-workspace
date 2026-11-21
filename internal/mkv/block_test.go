package mkv

import (
	"bytes"
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
)

// header builds a block header with a single-byte track-number vint.
func header(track byte, rel int16, flags byte) []byte {
	if track >= 0x80 {
		panic("test track number must fit a one-byte vint")
	}
	return []byte{0x80 | track, byte(rel >> 8), byte(rel), flags}
}

func TestSimpleBlockNoLacing(t *testing.T) {
	payload := append(header(1, 20, flagKeyframe), []byte("frame")...)
	b, err := ParseSimpleBlock(payload, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if b.TrackNumber != 1 || b.RelativeTimestamp != 20 || !b.Keyframe || len(b.Frames) != 1 {
		t.Fatalf("unexpected block: %+v", b)
	}
	if b.Frames[0].SourceOffset != 1004 || b.Frames[0].Size != 5 {
		t.Fatalf("frame offset=%d size=%d", b.Frames[0].SourceOffset, b.Frames[0].Size)
	}
	if !bytes.Equal(b.Frames[0].Data, []byte("frame")) {
		t.Fatalf("frame data lost")
	}
}

func TestSimpleBlockNegativeRelativeTimestamp(t *testing.T) {
	payload := append(header(2, -7, 0), []byte("x")...)
	b, err := ParseSimpleBlock(payload, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.RelativeTimestamp != -7 {
		t.Fatalf("rel=%d", b.RelativeTimestamp)
	}
}

func TestXiphLacing(t *testing.T) {
	// 3 frames of 2, 300, 1 bytes. Sizes are 255-chained in the header.
	var lace []byte
	lace = append(lace, 0x02) // frame count - 1
	lace = append(lace, 2)    // frame 0 size
	lace = append(lace, 255, 45)
	lace = append(lace, 'a', 'b')
	lace = append(lace, bytes.Repeat([]byte{'c'}, 300)...)
	lace = append(lace, 'd')
	payload := append(header(3, 0, LacingXiph<<1), lace...)
	b, err := ParseSimpleBlock(payload, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Frames) != 3 {
		t.Fatalf("frames=%d", len(b.Frames))
	}
	if b.Frames[0].Size != 2 || b.Frames[1].Size != 300 || b.Frames[2].Size != 1 {
		t.Fatalf("sizes=%d,%d,%d", b.Frames[0].Size, b.Frames[1].Size, b.Frames[2].Size)
	}
	if b.Frames[1].SourceOffset != 500+4+6 {
		t.Fatalf("offset=%d", b.Frames[1].SourceOffset)
	}
}

func TestFixedLacing(t *testing.T) {
	lace := append([]byte{2}, []byte("aabbcc")...)
	payload := append(header(1, 0, LacingFixed<<1), lace...)
	b, err := ParseSimpleBlock(payload, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Frames) != 3 {
		t.Fatalf("frames=%d", len(b.Frames))
	}
	for _, f := range b.Frames {
		if f.Size != 2 {
			t.Fatalf("size=%d", f.Size)
		}
	}
}

func TestEBMLLacing(t *testing.T) {
	// 3 frames of 4, 8, 6 bytes. The first size is 0x84; a single delta of +4
	// (0xC3) covers the second frame and the third is implied by the remainder.
	lace := []byte{0x02, 0x84, 0xC3}
	lace = append(lace, bytes.Repeat([]byte{'x'}, 4)...)
	lace = append(lace, bytes.Repeat([]byte{'y'}, 8)...)
	lace = append(lace, bytes.Repeat([]byte{'z'}, 6)...)
	payload := append(header(1, 0, LacingEBML<<1), lace...)
	b, err := ParseSimpleBlock(payload, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Frames) != 3 || b.Frames[0].Size != 4 || b.Frames[1].Size != 8 || b.Frames[2].Size != 6 {
		t.Fatalf("unexpected frames: %+v", b.Frames)
	}
}

func TestBlockTooShort(t *testing.T) {
	_, err := ParseSimpleBlock([]byte{0x81, 0x00}, 42)
	if !errors.Is(err, ErrBlock) {
		t.Fatalf("got %v", err)
	}
	if fault.OffsetOf(err) != 42 {
		t.Fatalf("offset=%d", fault.OffsetOf(err))
	}
}

func TestLacingTruncated(t *testing.T) {
	payload := append(header(1, 0, LacingXiph<<1), 5, 10, 'a')
	_, err := ParseSimpleBlock(payload, 0)
	if !errors.Is(err, ErrLacing) {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyFrameRejected(t *testing.T) {
	payload := header(1, 0, 0)
	_, err := ParseSimpleBlock(payload, 0)
	if !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("got %v", err)
	}
}

func TestTrackNumberMultiByte(t *testing.T) {
	// Track 300 encodes as the two-byte vint 0x412C.
	payload := []byte{0x41, 0x2C, 0x00, 0x00, 0x80, 'z'}
	b, err := ParseSimpleBlock(payload, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.TrackNumber != 300 {
		t.Fatalf("track=%d", b.TrackNumber)
	}
}

func TestBlockGroupBlockKeyframeFromFlag(t *testing.T) {
	payload := append(header(4, 0, 0), []byte("v")...)
	b, err := ParseBlock(payload, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Keyframe {
		t.Fatal("BlockGroup keyframe flag lost")
	}
}
