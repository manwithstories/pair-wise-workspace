package demux

import (
	"testing"

	"mediaproc/internal/ebml"
)

func TestXiphLacing(t *testing.T) {
	// Four-byte block header placeholder, then count=2 means three frames and
	// two explicit sizes 2 and 3. Remaining bytes are the final frame.
	payload := []byte{
		0, 0, 0, 0,
		2,        // three frames
		2,        // first size
		3,        // second size
		'a', 'b', // frame 0
		'c', 'd', 'e', // frame 1
		'f', 'g', 'h', 'i', // frame 2
	}
	frames, err := parseLacedFrames(payload, 4, ebml.LacingXiph)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || len(frames[0]) != 2 || len(frames[1]) != 3 || len(frames[2]) != 4 {
		t.Fatalf("frames=%v", frames)
	}
}

func TestInvalidLacingIsRejected(t *testing.T) {
	payload := []byte{0, 0, 0, 0, 5, 255, 255, 255}
	if _, err := parseLacedFrames(payload, 4, ebml.LacingXiph); err == nil {
		t.Fatal("expected truncated Xiph lacing error")
	}
	if _, err := parseLacedFrames([]byte{0, 0, 0, 0, 1, 2}, 4, ebml.LacingFixed); err == nil {
		t.Fatal("expected uneven fixed-lacing error")
	}
}
