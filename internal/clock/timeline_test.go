package clock

import (
	"math"
	"testing"
)

func TestTicksConversion(t *testing.T) {
	if got := Ticks(90000, 90000); got != 90000 {
		t.Fatalf("identity: got %d", got)
	}
	if got := Ticks(48000, 48000); got != 90000 {
		t.Fatalf("1s at 48k: got %d", got)
	}
	if got := Ticks(1000, 1000); got != 90000 {
		t.Fatalf("1s at 1k: got %d", got)
	}
	// Rounding: 1 tick at 44100 = 2.0408 canonical ticks -> 2.
	if got := Ticks(1, 44100); got != 2 {
		t.Fatalf("rounding: got %d", got)
	}
	if got := Ticks(0, 0); got != 0 {
		t.Fatalf("zero scale: got %d", got)
	}
}

func TestMillisRoundTrip(t *testing.T) {
	ticks := TicksFromMillis(2033)
	if ms := Millis(ticks); ms < 2032.9 || ms > 2033.1 {
		t.Fatalf("round trip: got %f", ms)
	}
}

func TestRTPWraparound(t *testing.T) {
	// 2^32 ticks at 90 kHz must wrap to 0.
	if got := RTP(math.MaxUint32+1, 90000); got != 0 {
		t.Fatalf("wrap to 0: got %d", got)
	}
	if got := RTP(math.MaxUint32+6, 90000); got != 5 {
		t.Fatalf("wrap to 5: got %d", got)
	}
}

func TestCheckRTPSequence(t *testing.T) {
	bounds := []int64{0, 180000, 360000, 540000}
	if err := CheckRTPSequence(bounds, 90000, 96); err != nil {
		t.Fatalf("valid sequence: %v", err)
	}
	// Clock rate 1 collapses distinct boundaries onto one timestamp.
	if err := CheckRTPSequence([]int64{0, 1, 2}, 1, 96); err == nil {
		t.Fatal("expected collision error")
	}
	if err := CheckRTPSequence(bounds, 0, 96); err == nil {
		t.Fatal("expected zero-rate error")
	}
}
