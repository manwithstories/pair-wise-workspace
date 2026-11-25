package audio

import (
	"encoding/binary"
	"math"
	"testing"

	"streamcut/internal/media"
)

// synthPCM mirrors the fixture's tone/silence schedule so the test exercises
// the detector without importing the fixture package.
func synthPCM(frame int, n int) []byte {
	pcm := make([]byte, n*2)
	silent := (frame >= 100 && frame < 150) || (frame >= 250 && frame < 350)
	if silent {
		for i := 0; i < n; i++ {
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16((i+frame)&1))
		}
		return pcm
	}
	for i := 0; i < n; i++ {
		v := int16(12000 * math.Sin(2*math.Pi*440/48000*float64(i)))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	return pcm
}

func TestDetectsSilenceWithHighF1(t *testing.T) {
	cfg := DefaultConfig()
	d := NewDetector(cfg)
	tickPerFrame := media.Time90(cfg.WindowSamples) *
		media.TicksPerSecond / media.Time90(cfg.SampleRate)

	// Ground truth intervals in ticks.
	ref := []Interval{
		{Start: 100 * tickPerFrame, End: 150 * tickPerFrame},
		{Start: 250 * tickPerFrame, End: 350 * tickPerFrame},
	}
	for f := 0; f < 500; f++ {
		pts := media.Time90(f+1) * tickPerFrame
		d.PushPCM(synthPCM(f, cfg.WindowSamples), pts)
	}
	got := d.Flush()
	if len(got) != 2 {
		t.Fatalf("detected %d intervals, want 2: %+v", len(got), got)
	}
	s := Score(got, ref, tickPerFrame)
	if s.F1 < 0.95 {
		t.Fatalf("silence F1 = %.3f, want >= 0.95 (P=%.3f R=%.3f)",
			s.F1, s.Precision, s.Recall)
	}
}

func TestHysteresisIgnoresOneWindowBlip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnterWindows = 3
	d := NewDetector(cfg)
	tick := media.Time90(cfg.WindowSamples) *
		media.TicksPerSecond / media.Time90(cfg.SampleRate)
	loud := synthPCM(0, cfg.WindowSamples)
	quiet := synthPCM(120, cfg.WindowSamples)
	// Ten loud windows, one quiet blip, then loud again: must not open.
	seq := [][]byte{}
	for i := 0; i < 10; i++ {
		seq = append(seq, loud)
	}
	seq = append(seq, quiet)
	for i := 0; i < 10; i++ {
		seq = append(seq, loud)
	}
	for i, p := range seq {
		d.PushPCM(p, media.Time90(i+1)*tick)
	}
	if iv := d.Flush(); len(iv) != 0 {
		t.Fatalf("single blip opened a silence interval: %+v", iv)
	}
}

func TestUnsupportedCodecNeverRunsSilence(t *testing.T) {
	// The detector only consumes PCM16LE. An unsupported codec must be
	// rejected upstream and never reach here; document the contract by
	// confirming zero-input yields zero intervals rather than false silence.
	d := NewDetector(DefaultConfig())
	if iv := d.Flush(); len(iv) != 0 {
		t.Fatalf("empty input must not be silence: %+v", iv)
	}
}
