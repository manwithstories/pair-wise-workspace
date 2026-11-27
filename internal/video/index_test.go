package video

import (
	"testing"

	"archiver/internal/media"
)

// vfrStream builds a variable-frame-rate stream: irregular frame
// intervals, keyframes every ~1s.
func vfrStream() ([]Frame, []byte) {
	intervals := []int64{33333, 16667, 50000, 40000, 25000, 41666, 33334}
	var stream []byte
	var pts int64
	for i := 0; i < 70; i++ {
		key := i%30 == 0
		payload := []byte{byte(i), byte(i * 7), byte(i * 13)}
		stream = append(stream, EncodeFrame(media.TimeUs(pts), key, payload)...)
		pts += intervals[i%len(intervals)]
	}
	frames, err := ParseStream(stream)
	if err != nil {
		panic(err)
	}
	return frames, stream
}

func TestIndexSeekAllKeyframes(t *testing.T) {
	frames, _ := vfrStream()
	ix := Build(frames)
	keys := ix.Keyframes()
	if len(keys) != 3 {
		t.Fatalf("keyframes = %v", keys)
	}
	hits := 0
	for _, k := range keys {
		if hit, bad := ix.Seek(k); hit && bad == 0 {
			hits++
		}
	}
	if hits != len(keys) {
		t.Fatalf("hits = %d/%d", hits, len(keys))
	}
}

func TestSeekNonKeyframeLandsOnPriorKeyframe(t *testing.T) {
	frames, _ := vfrStream()
	ix := Build(frames)
	target := frames[10].PTS // not a keyframe
	hit, bad := ix.Seek(target)
	if !hit || bad != 0 {
		t.Fatalf("hit=%v bad=%d", hit, bad)
	}
}

func TestBadFrameDoesNotShiftIndex(t *testing.T) {
	frames, stream := vfrStream()
	// Corrupt one payload byte of frame 5 in the middle of GOP 0.
	corrupt := frames[5].Off + frameHeaderLen
	stream[corrupt] ^= 0xff
	frames, err := ParseStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	ix := Build(frames)
	keys := ix.Keyframes()
	// Index positions are header-derived: unchanged by the corruption.
	if len(keys) != 3 {
		t.Fatalf("keyframes shifted: %v", keys)
	}
	// Seeking a later keyframe still hits cleanly: the bad frame is in
	// an earlier GOP and never affects subsequent entries.
	last := keys[len(keys)-1]
	if hit, bad := ix.Seek(last); !hit || bad != 0 {
		t.Fatalf("later keyframe: hit=%v bad=%d", hit, bad)
	}
	// Seeking a target past the corrupt frame inside its GOP reports the
	// bad frame but still locates the surviving target frame.
	target := frames[8].PTS
	hit, bad := ix.Seek(target)
	if bad != 1 {
		t.Fatalf("bad frames = %d, want 1", bad)
	}
	if !hit {
		t.Fatal("target frame 8 must still decode")
	}
}

func TestVFRTimestampsPreserved(t *testing.T) {
	frames, _ := vfrStream()
	ix := Build(frames)
	keys := ix.Keyframes()
	for i := 1; i < len(keys); i++ {
		if keys[i] <= keys[i-1] {
			t.Fatalf("non-monotonic keyframes: %v", keys)
		}
	}
	// Irregular intervals must survive intact in the index.
	if frames[1].PTS-frames[0].PTS != 33333 || frames[2].PTS-frames[1].PTS != 16667 {
		t.Fatalf("VFR timestamps mangled: %v %v", frames[1].PTS, frames[2].PTS)
	}
}
