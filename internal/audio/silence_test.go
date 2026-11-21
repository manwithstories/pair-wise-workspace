package audio

import (
	"encoding/binary"
	"testing"
)

// pcm builds an interleaved 16-bit stereo buffer from per-channel
// constant amplitudes, one entry per frame.
func pcm(t *testing.T, frames [] [2]int16) []byte {
	t.Helper()
	buf := make([]byte, len(frames)*4)
	for i, f := range frames {
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(f[0]))
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(f[1]))
	}
	return buf
}

func TestDetectSilencePerChannel(t *testing.T) {
	// 100 frames at 100 Hz, 10 ms frames → 1 sample per frame.
	// Left channel loud, right channel silent for frames 20..40.
	frames := make([][2]int16, 100)
	for i := range frames {
		frames[i] = [2]int16{10000, 10000}
	}
	for i := 20; i < 40; i++ {
		frames[i][1] = 0
	}
	rep := Detect("audio", pcm(t, frames), 2, 100, -50, 10, 0)
	if len(rep.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", rep.Issues)
	}
	var rightSilent *Interval
	for i := range rep.Intervals {
		if rep.Intervals[i].Channel == 1 {
			rightSilent = &rep.Intervals[i]
		}
	}
	if rightSilent == nil {
		t.Fatalf("no silent interval on right channel: %+v", rep.Intervals)
	}
	if rightSilent.StartMs != 200 || rightSilent.EndMs != 400 {
		t.Fatalf("interval=[%v,%v) want [200,400)", rightSilent.StartMs, rightSilent.EndMs)
	}
	// Left channel stays loud everywhere.
	for _, iv := range rep.Intervals {
		if iv.Channel == 0 {
			t.Fatalf("left channel wrongly silent: %+v", iv)
		}
	}
}

func TestDetectEmptyTrack(t *testing.T) {
	rep := Detect("audio", nil, 2, 16000, -50, 10, 0)
	if len(rep.Issues) != 1 {
		t.Fatalf("issues=%v want 1 empty-track entry", rep.Issues)
	}
	if rep.TotalFrames != 0 {
		t.Fatalf("frames=%d want 0", rep.TotalFrames)
	}
}

func TestDetectTruncatedBytes(t *testing.T) {
	buf := pcm(t, make([][2]int16, 10))
	buf = append(buf, 0xAB) // dangling byte
	rep := Detect("audio", buf, 2, 100, -50, 10, 0)
	if len(rep.Issues) != 1 {
		t.Fatalf("issues=%v want 1 truncation entry", rep.Issues)
	}
	if rep.TotalFrames != 10 {
		t.Fatalf("frames=%d want 10 (truncated byte ignored)", rep.TotalFrames)
	}
}

func TestDetectAllSilent(t *testing.T) {
	rep := Detect("audio", make([]byte, 400), 2, 100, -50, 10, 0)
	if !rep.AllSilent() {
		t.Fatalf("AllSilent=false, silent=%d total=%d", rep.SilentFrames, rep.TotalFrames)
	}
	if len(rep.Intervals) != 2 { // one merged interval per channel
		t.Fatalf("intervals=%d want 2", len(rep.Intervals))
	}
}

func TestThresholdBoundaryIsLocatable(t *testing.T) {
	// Amplitude chosen so RMS sits exactly at the threshold:
	// 20*log10(A/32768) = -40 → A = 32768 * 10^-2 ≈ 327.68 → 328.
	frames := make([][2]int16, 10)
	for i := range frames {
		frames[i] = [2]int16{328, 328}
	}
	rep := Detect("audio", pcm(t, frames), 2, 100, -40, 10, 0)
	if len(rep.BoundaryHits) == 0 {
		t.Fatal("threshold-boundary frame not reported")
	}
}

func TestInvalidParameters(t *testing.T) {
	rep := Detect("audio", []byte{1, 2}, 0, 16000, -50, 10, 0)
	if len(rep.Issues) != 1 {
		t.Fatalf("issues=%v want parameter error", rep.Issues)
	}
}
