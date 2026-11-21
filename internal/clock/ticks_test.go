package clock

import "testing"

func TestScaleExactAndRounding(t *testing.T) {
	cases := []struct {
		name                       string
		value, srcRate, dstRate, want int64
	}{
		{"25fps frame to 90k ticks", 1, 25, TicksPerSecond, 3600},
		{"one second 48k audio to ticks", 48000, 48000, TicksPerSecond, TicksPerSecond},
		{"half tick rounding up", 1, 180000, TicksPerSecond, 1},
		{"zero on bad rate", 7, 0, TicksPerSecond, 0},
		{"ticks to milliseconds", TicksPerSecond, TicksPerSecond, 1000, 1000},
	}
	for _, tc := range cases {
		if got := Scale(tc.value, tc.srcRate, tc.dstRate); got != tc.want {
			t.Errorf("%s: Scale = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestToFromTicksRoundTrip(t *testing.T) {
	// 30 fps presentation time stamps across one hour.
	const frames int64 = 30 * 3600
	ticks := ToTicks(frames, 30)
	if ticks != TicksPerSecond*3600 {
		t.Fatalf("30fps one hour = %d ticks, want %d", ticks, TicksPerSecond*3600)
	}
	if back := FromTicks(ticks, 30); back != frames {
		t.Fatalf("round trip = %d frames, want %d", back, frames)
	}
	// Negative edit-list media time must survive conversion.
	neg := ToTicks(-3003, 90000)
	if neg >= 0 || FromTicks(neg, 90000) != -3003 {
		t.Fatalf("negative media time not preserved: %d", neg)
	}
}

func TestNoAccumulatedDrift(t *testing.T) {
	// Summing per-frame conversions for 24fps must equal the whole-span
	// conversion: this is the drift guarantee the manifest relies on.
	const frames = 24 * 7200
	var summed int64
	for i := int64(0); i < frames; i++ {
		summed += ToTicks(1, 24)
	}
	whole := ToTicks(frames, 24)
	if summed != whole {
		t.Fatalf("accumulated 24fps ticks %d != whole span %d (drift=%d)", summed, whole, summed-whole)
	}
}

func TestReduce(t *testing.T) {
	if got := Reduce(Rational{Num: 48000, Den: 90000}); got != (Rational{Num: 8, Den: 15}) {
		t.Fatalf("reduce = %+v", got)
	}
	if got := GCD(0, 9); got != 9 {
		t.Fatalf("gcd(0,9) = %d", got)
	}
}
