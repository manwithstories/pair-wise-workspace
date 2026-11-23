package clock_test

import (
	"testing"

	"media-pipeline/internal/clock"
)

func TestTimebaseConversions(t *testing.T) {
	tb := clock.Timebase{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	if err := tb.Valid(); err != nil {
		t.Fatalf("valid timebase rejected: %v", err)
	}
	if got := tb.FrameSize(); got != 4 {
		t.Fatalf("frame size = %d, want 4", got)
	}
	if got := tb.FramesToBytes(10); got != 40 {
		t.Fatalf("frames->bytes = %d, want 40", got)
	}
	frames, _, err := tb.BytesToFrames(40)
	if err != nil || frames != 10 {
		t.Fatalf("bytes->frames = (%d,%v), want 10", frames, err)
	}
	if _, rem, err := tb.BytesToFrames(41); err == nil || rem != 1 {
		t.Fatalf("unaligned offset: err=%v rem=%d, want rem=1", err, rem)
	}
	if got := tb.FramesToMillis(48000); got != 1000 {
		t.Fatalf("frames->ms = %d, want 1000", got)
	}
	if got := tb.MillisToFrames(1000); got != 48000 {
		t.Fatalf("ms->frames = %d, want 48000", got)
	}
	if got := tb.Summary(); got != "48000/2/16" {
		t.Fatalf("summary = %q", got)
	}
}

func TestTimebaseInvalid(t *testing.T) {
	cases := []clock.Timebase{
		{SampleRate: 0, Channels: 2, BitsPerSample: 16},
		{SampleRate: 48000, Channels: 0, BitsPerSample: 16},
		{SampleRate: 48000, Channels: 2, BitsPerSample: 12},
	}
	for i, tb := range cases {
		if err := tb.Valid(); err == nil {
			t.Fatalf("case %d: invalid timebase accepted", i)
		}
	}
}
