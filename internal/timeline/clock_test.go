package timeline

import "testing"

func TestConvertRoundTrip(t *testing.T) {
	// 1 second of audio <-> video ticks.
	if got := Convert(48000, Audio, Video); got != 90000 {
		t.Fatalf("48000 audio ticks = %d video ticks, want 90000", got)
	}
	if got := Convert(90000, Video, Audio); got != 48000 {
		t.Fatalf("90000 video ticks = %d audio ticks, want 48000", got)
	}
	// One video frame at 30fps -> audio ticks (3000 -> 1600).
	if got := Convert(3000, Video, Audio); got != 1600 {
		t.Fatalf("3000 video ticks = %d audio ticks, want 1600", got)
	}
}

func TestConvertRounding(t *testing.T) {
	// 1 audio tick = 1.875 video ticks -> rounds to 2.
	if got := Convert(1, Audio, Video); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
	// 1 video tick = 0.5333 audio ticks -> rounds to 1.
	if got := Convert(1, Video, Audio); got != 1 {
		t.Fatalf("got %d, want 1", got)
	}
	// Negative values round half away from zero.
	if got := Convert(-1, Audio, Video); got != -2 {
		t.Fatalf("got %d, want -2", got)
	}
}

func TestConvertLargeNoOverflow(t *testing.T) {
	// ~1 year of 90kHz ticks must not overflow.
	v := int64(90000) * 3600 * 24 * 365
	if got := Convert(v, Video, Millis); got != v/90 {
		t.Fatalf("got %d, want %d", got, v/90)
	}
}

func TestAbsDiffMillis(t *testing.T) {
	// Audio frame at 48000 ticks (1s) vs video at 90045 ticks (1.0005s).
	d := AbsDiffMillis(48000, Audio, 90045, Video)
	if d < 0.49 || d > 0.51 {
		t.Fatalf("diff %f, want ~0.5ms", d)
	}
}

func TestIsFrameAligned(t *testing.T) {
	if !IsFrameAligned(6000, Video, 3000) {
		t.Fatal("6000 should align to 3000-tick frames")
	}
	if IsFrameAligned(6001, Video, 3000) {
		t.Fatal("6001 should not align")
	}
	if IsFrameAligned(10, Video, 0) {
		t.Fatal("zero frame duration never aligns")
	}
}

func TestErrorFormat(t *testing.T) {
	e := NewSeq(ErrGap, "seg-3.m4s", 42, 1000, 1040, "burst loss")
	want := "unrecoverable_gap: seg-3.m4s seq=42 [1000ms,1040ms]: burst loss"
	if e.Error() != want {
		t.Fatalf("got %q, want %q", e.Error(), want)
	}
}
