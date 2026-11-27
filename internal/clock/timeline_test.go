package clock

import "testing"

func TestRationalConversionRoundTrip(t *testing.T) {
	tb := Rational{Num: 1, Den: 48000}
	for _, ts := range []int64{0, 4800, 48000, 96001, 1234567} {
		ticks := ToTicks(ts, tb)
		back := TicksToRational(ticks, tb)
		if back != ts {
			t.Fatalf("round trip mismatch ts=%d ticks=%d back=%d", ts, ticks, back)
		}
	}
}

func TestMsConversions(t *testing.T) {
	if got := Ms(90000); got != 1000 {
		t.Fatalf("90000 ticks = %d ms, want 1000", got)
	}
	if got := FromMs(1000); got != 90000 {
		t.Fatalf("1000 ms = %d ticks, want 90000", got)
	}
	if Ms(45) != 1 { // 0.5ms rounds to 1
		t.Fatalf("45 ticks = %d ms, want 1", Ms(45))
	}
}

func TestTimelineMonotonicOnReset(t *testing.T) {
	tl := NewTimeline()
	raw := []int64{0, 90000, 180000, 5000, 90000, 180000}
	var prev int64 = -1
	for i, r := range raw {
		got := tl.Advance(r)
		if i > 0 && got < prev {
			t.Fatalf("cut point moved backwards at %d: %d < %d", i, got, prev)
		}
		prev = got
	}
	if tl.Resets() != 1 {
		t.Fatalf("resets = %d, want 1", tl.Resets())
	}
	// After the reset the adjusted timeline continues after the prior peak.
	if tl.Last() <= 180000 {
		t.Fatalf("last = %d should exceed pre-reset peak 180000", tl.Last())
	}
}

func TestTimelineWraparound(t *testing.T) {
	tl := NewTimeline()
	high := int64(1<<34) - 100
	seq := []int64{high - 10, high, 50, 150, 250}
	var prev int64
	for i, r := range seq {
		got := tl.Advance(r)
		if i > 0 && got < prev {
			t.Fatalf("wraparound moved backwards at %d: %d < %d", i, got, prev)
		}
		prev = got
	}
	if tl.Wraparounds() != 1 {
		t.Fatalf("wraparounds = %d, want 1", tl.Wraparounds())
	}
}

func TestAlignVideoAndAudio(t *testing.T) {
	// 30 fps => a frame spans 3000 ticks.
	fr := Rational{Num: 30, Den: 1}
	if got := AlignVideo(7500, fr); got != 6000 {
		t.Fatalf("video align = %d, want 6000", got)
	}
	// 48 kHz audio: one sample = 90000/48000 ticks (1.875).
	ticks := FromMs(20) // 1800 ticks
	aligned := AlignAudio(ticks, 48000)
	if aligned > ticks {
		t.Fatalf("audio align rounded up: %d > %d", aligned, ticks)
	}
	// aligned must sit on an exact sample boundary.
	sample := aligned * 48000 / 90000
	if sample*90000/48000 != aligned {
		t.Fatalf("aligned %d is not on a sample boundary", aligned)
	}
}

func TestNewRationalRejectsZeroDen(t *testing.T) {
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("expected error for zero denominator")
	}
}
