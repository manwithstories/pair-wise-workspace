package mp4clip

import (
	"bytes"
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

func buildTestClip(isVideo bool) Clip {
	rate := uint32(clock.AudioClockRate)
	if isVideo {
		rate = clock.VideoClockRate
	}
	samples := make([]Sample, 8)
	for i := range samples {
		samples[i] = Sample{
			Delta: rate / 30,
			Data:  []byte{byte(i), byte(i + 1), byte(i + 2), byte(i + 3)},
		}
	}
	return Clip{
		TrackID:   1,
		Timescale: rate,
		IsVideo:   isVideo,
		Samples:   samples,
		Width:     320,
		Height:    240,
	}
}

func TestWriteReadTimingRoundTrip(t *testing.T) {
	for _, isVideo := range []bool{false, true} {
		clip := buildTestClip(isVideo)
		buf := NewMemBuffer()
		if err := WriteClip(buf, clip); err != nil {
			t.Fatalf("write: %v", err)
		}
		timing, err := ReadTiming(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if timing.Timescale != clip.Timescale {
			t.Fatalf("timescale: got %d want %d", timing.Timescale, clip.Timescale)
		}
		if isVideo && timing.Handler != "vide" || !isVideo && timing.Handler != "soun" {
			t.Fatalf("handler type wrong: %q", timing.Handler)
		}
		wantDeltas := make([]uint32, len(clip.Samples))
		wantSizes := make([]uint32, len(clip.Samples))
		for i, s := range clip.Samples {
			wantDeltas[i] = s.Delta
			wantSizes[i] = uint32(len(s.Data))
		}
		if err := timing.VerifyTiming(wantDeltas, wantSizes); err != nil {
			t.Fatalf("verify: %v", err)
		}
		// Cumulative sample times must be deterministic and monotonic.
		if timing.SampleTimes[1] != wantDeltas[0] {
			t.Fatal("cumulative sample start wrong")
		}
		var sum uint32
		for _, d := range wantDeltas {
			sum += d
		}
		if timing.TotalDuration != sum {
			t.Fatalf("total duration: got %d want %d", timing.TotalDuration, sum)
		}
	}
}

func TestRejectsEmptyClip(t *testing.T) {
	err := WriteClip(NewMemBuffer(), Clip{Timescale: 48000})
	if err == nil {
		t.Fatal("empty clip must be rejected")
	}
	if merr, ok := err.(*model.MediaError); ok && merr.Category != model.CatAlignment {
		t.Fatalf("error category wrong: %v", merr)
	}
}

func TestTruncatedClipFailsParse(t *testing.T) {
	buf := NewMemBuffer()
	if err := WriteClip(buf, buildTestClip(false)); err != nil {
		t.Fatal(err)
	}
	truncated := buf.Bytes()[:buf.Len()-40]
	if _, err := ReadTiming(bytes.NewReader(truncated)); err == nil {
		t.Fatal("truncated mp4 must fail timing parse")
	}
}
