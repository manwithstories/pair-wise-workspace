package clock

import (
	"math"
	"testing"

	"mediacheck/internal/model"
)

func TestTicksToMs(t *testing.T) {
	tl := New()
	tl.AddTrack("video", 90000)
	cases := []struct {
		ticks int64
		want  int64
	}{
		{0, 0},
		{90000, 1000},
		{45000, 500},
	}
	for _, c := range cases {
		got, ok := tl.TicksToMs("video", c.ticks)
		if !ok || got != c.want {
			t.Fatalf("ticks=%d got=(%d,%v) want=%d", c.ticks, got, ok, c.want)
		}
	}
}

func TestTicksToMsRejectsBadInput(t *testing.T) {
	tl := New()
	tl.AddTrack("zero", 0)
	tl.AddTrack("good", 1000)

	if _, ok := tl.TicksToMs("good", -1); ok {
		t.Fatal("negative duration must be rejected")
	}
	if _, ok := tl.TicksToMs("zero", 100); ok {
		t.Fatal("zero timescale conversion must fail")
	}
	if _, ok := tl.TicksToMs("ghost", 100); ok {
		t.Fatal("unregistered track must fail")
	}
	if _, ok := tl.TicksToMs("good", math.MaxInt64); ok {
		t.Fatal("overflowing timestamp must be rejected")
	}
	// AddTrack("zero", 0) itself raises one fault, then four failed
	// conversions raise one each, for five total.
	faults := tl.Faults()
	if len(faults) != 5 {
		t.Fatalf("want 5 timebase faults, got %d", len(faults))
	}
	for _, f := range faults {
		if f.Kind != model.KindInvalidTimebase {
			t.Fatalf("unexpected fault kind %s", f.Kind)
		}
	}
}

func TestRTPWrap(t *testing.T) {
	tl := New()
	tl.AddTrack("v", 90000)
	tl.SetRTPClock("v", 90000)

	// Start near the 32-bit ceiling then cross it.
	base := uint32(0xFFFFFFFF - 90000 + 1)
	first, ok := tl.RTPToMs("v", base)
	if !ok || first != 0 {
		t.Fatalf("first RTP sample got %d,%v", first, ok)
	}
	// Exactly one clock second later, after a wrap to a small value.
	second, ok := tl.RTPToMs("v", 1)
	if !ok {
		t.Fatal("post-wrap sample rejected")
	}
	if second < 990 || second > 1010 {
		t.Fatalf("wrapped timestamp should map near 1000ms, got %d", second)
	}
}

func TestRTPLatePacketResolvesToPast(t *testing.T) {
	tl := New()
	tl.AddTrack("v", 1000)
	tl.SetRTPClock("v", 1000)
	_, _ = tl.RTPToMs("v", 1000) // anchor at 1s-worth of ticks
	high, _ := tl.RTPToMs("v", 5000)
	if high != 4000 {
		t.Fatalf("high sample want 4000ms got %d", high)
	}
	// A late packet with a small raw value must not look like a wrap.
	late, ok := tl.RTPToMs("v", 2000)
	if !ok || late != 1000 {
		t.Fatalf("late packet want 1000ms got %d,%v", late, ok)
	}
}

func TestDrift(t *testing.T) {
	if Drift(120, 100) != 20 || Drift(100, 120) != 20 {
		t.Fatal("Drift must return absolute difference")
	}
}
