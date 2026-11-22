package clock

import "testing"

func TestMillisecondRoundTrip(t *testing.T) {
	for _, ms := range []int64{0, 1, 20, 180, 1000, -35} {
		got := Milliseconds(ms).Milliseconds()
		if got != ms {
			t.Fatalf("ms round trip: want %d got %d", ms, got)
		}
	}
}

func TestRTPMapping(t *testing.T) {
	// One audio second at 48kHz must map to one unified second.
	if got := FromRTP(48000, AudioClockRate); got != Ticks(TicksPerSecond) {
		t.Fatalf("audio rtp mapping: %v", got)
	}
	// One video second at 90kHz must map to one unified second.
	if got := FromRTP(90000, VideoClockRate); got != Ticks(TicksPerSecond) {
		t.Fatalf("video rtp mapping: %v", got)
	}
	// Round trip is exact for audio because rates coincide.
	v := ToRTP(FromSeconds(2.5), AudioClockRate)
	if v != 120000 {
		t.Fatalf("audio round trip: %d", v)
	}
}

func TestFromSeconds(t *testing.T) {
	if FromSeconds(1) != Ticks(TicksPerSecond) {
		t.Fatal("one second mismatch")
	}
	if FromSeconds(0) != 0 {
		t.Fatal("zero mismatch")
	}
}
