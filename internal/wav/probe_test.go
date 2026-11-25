package wav

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFrameAlignedTruncationKeepsPrefixReadable confirms that truncating a WAV
// to a frame-aligned smaller size still opens and leaves the surviving prefix
// fully inspectable, while the reader exposes frame metadata for the caller to
// detect the shortfall against the expected count.
func TestFrameAlignedTruncationKeepsPrefixReadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fa.wav")
	frames := TileFrames * 3
	data := pcmTiles(frames, 2, 16, 0x33)
	buildWAV(t, p, 48000, 2, 16, data, nil)

	// Drop exactly one stereo 16-bit frame (4 bytes) from the data payload.
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw[:len(raw)-4], 0o644); err != nil {
		t.Fatal(err)
	}
	rd, err := NewReader(p)
	if err != nil {
		t.Fatalf("frame-aligned truncation must stay readable: %v", err)
	}
	defer rd.Close()
	if rd.Header().FrameCount >= int64(frames) {
		t.Fatalf("expected fewer frames after truncation, got %d", rd.Header().FrameCount)
	}
	if _, err := rd.Inspect(); err != nil {
		t.Fatalf("inspect must tolerate the shortened payload: %v", err)
	}
}
