package audio

import (
	"testing"

	"github.com/asticode/go-astiav"
)

// TestNormaliserOutputFormat pins the delivery contract: the graph output
// is 48 kHz interleaved float stereo even though loudnorm measures in
// double at 192 kHz internally.
func TestNormaliserOutputFormat(t *testing.T) {
	l, err := NewLoudnessNormaliser(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := toneF32(960, 2, 1000, 0.4, 48000)
	if _, err := l.Process(in, 0); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got := l.sink.SampleFormat(); got != astiav.SampleFormatFlt {
		t.Fatalf("sink sample format = %v, want flt", got)
	}
	if got := l.sink.SampleRate(); got != 48000 {
		t.Fatalf("sink rate = %d, want 48000", got)
	}
	if got := l.sink.ChannelLayout().Channels(); got != 2 {
		t.Fatalf("sink channels = %d, want 2", got)
	}
	if _, err := l.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// TestNewLoudnessRejectsBadSpec guards constructor validation.
func TestNewLoudnessRejectsBadSpec(t *testing.T) {
	if _, err := NewLoudnessNormaliser(0, 2); err == nil {
		t.Fatal("zero rate must be rejected")
	}
	if _, err := NewLoudnessNormaliser(48000, 0); err == nil {
		t.Fatal("zero channels must be rejected")
	}
}
