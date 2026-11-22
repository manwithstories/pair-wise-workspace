package audio

import (
	"encoding/binary"
	"math"
	"testing"

	"mediaprobe/internal/mp4"
	"mediaprobe/internal/segment"
)

func pcm(amp float64, frames int) []byte {
	b := make([]byte, 2*frames)
	for i := 0; i < frames; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(i)/48000))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func ts(pts, dur int64, data []byte) segment.TimedSample {
	return segment.TimedSample{
		Sample:   mp4.Sample{Data: data, Size: uint32(len(data))},
		MediaPTS: pts, MediaDur: dur,
	}
}

func TestSilenceIntervalsOnMediaClock(t *testing.T) {
	d := NewDetector(0.001, 1000)
	samples := []segment.TimedSample{
		ts(0, 1920, pcm(0.3, 1024)),    // sounding
		ts(1920, 1920, pcm(0, 1024)),   // silent
		ts(3840, 1920, pcm(0, 1024)),   // silent
		ts(5760, 1920, pcm(0.3, 1024)), // sounding
		ts(7680, 1920, pcm(0, 1024)),   // silent
		ts(9600, 1920, pcm(0, 1024)),   // silent (trailing run)
	}
	sil, unk := d.Analyze(samples)
	if len(unk) != 0 {
		t.Fatalf("unknown=%v", unk)
	}
	if len(sil) != 2 {
		t.Fatalf("silence=%v", sil)
	}
	if sil[0].Start != 1920 || sil[0].End != 5760 {
		t.Fatalf("sil[0]=%+v", sil[0])
	}
	if sil[1].Start != 7680 || sil[1].End != 11520 {
		t.Fatalf("sil[1]=%+v", sil[1])
	}
}

func TestMinDurationFiltersShortRuns(t *testing.T) {
	d := NewDetector(0.001, 5000)
	samples := []segment.TimedSample{
		ts(0, 1920, pcm(0, 1024)),
		ts(1920, 1920, pcm(0.3, 1024)),
	}
	sil, _ := d.Analyze(samples)
	if len(sil) != 0 {
		t.Fatalf("sil=%v", sil)
	}
}

func TestUnknownIntervalsCarryReasons(t *testing.T) {
	d := NewDetector(0.001, 1000)
	gap := ts(0, 1920, pcm(0.3, 1024))
	gap.GapBefore = true
	unc := ts(1920, 1920, pcm(0.3, 1024))
	unc.Uncertain = true
	_, unk := d.Analyze([]segment.TimedSample{gap, unc})
	if len(unk) != 2 {
		t.Fatalf("unknown=%v", unk)
	}
	if unk[0].Reason == "" || unk[1].Reason == "" || unk[0].Reason == unk[1].Reason {
		t.Fatalf("reasons=%q,%q", unk[0].Reason, unk[1].Reason)
	}
}
