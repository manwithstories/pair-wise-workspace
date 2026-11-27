package clock

import (
	"math/big"
	"testing"
	"time"
)

func TestAudioAndVideoTimebases(t *testing.T) {
	audio := Audio(48000)
	if got := audio.String(); got != "1/48000" {
		t.Fatalf("audio timebase = %s, want 1/48000", got)
	}
	if got := audio.Nanos(48000); got != int64(time.Second) {
		t.Fatalf("one second of audio ticks = %d ns, want %d", got, int64(time.Second))
	}
	ntsc := Video(30000, 1001)
	if got := ntsc.String(); got != "1001/30000" {
		t.Fatalf("ntsc timebase = %s, want 1001/30000", got)
	}
	// 30000 frames of 29.97 fps is exactly 1001 seconds.
	if got := ntsc.Nanos(30000); got != int64(1001*time.Second) {
		t.Fatalf("ntsc span = %d ns, want %d", got, int64(1001*time.Second))
	}
}

func TestRescaleKeepsPrecision(t *testing.T) {
	pal := Video(25, 1) // 1/25 tick
	ntsc := Video(30000, 1001)
	// 25 PAL frames are 1 second, which is 30000/1001 NTSC frames.  Rounded
	// to nearest tick that is 30 ticks, an error of 29.97 us per second,
	// inherent to the target grid, not accumulated by the converter.
	if got := pal.Rescale(25, ntsc); got != 30 {
		t.Fatalf("25 PAL ticks rescaled to NTSC = %d, want 30", got)
	}
	// A common-multiple round trip must come back within half a tick: 1000
	// PAL frames map to 1199 NTSC ticks (rounding up), back to 1000.16
	// frames.
	if got := ntsc.Rescale(pal.Rescale(1000, ntsc), pal); got != 1000 {
		t.Fatalf("round trip = %d, want 1000", got)
	}
	// 25 NTSC ticks span 0.834 s, which is 20.85 PAL ticks, so rounding to
	// 21 demonstrates the converter keeps the exact rational and does not
	// treat 29.97 as 30.
	if got := ntsc.Rescale(25, pal); got != 21 {
		t.Fatalf("25 NTSC ticks = %d PAL ticks, want 21", got)
	}
}

func TestAccumulatorStaysUnderOneMillisecond(t *testing.T) {
	cases := []struct {
		name string
		tb   Timebase
		tick int64
		n    int
	}{
		{"aac-1024@44100", Audio(44100), 1024, 200000},
		{"opus-960@48000", Audio(48000), 960, 200000},
		{"ntsc-frame", Video(30000, 1001), 1, 100000},
		{"ntsc-long-frame", Video(24000, 1001), 1, 100000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := NewAccumulator(tc.tb)
			var elapsed time.Duration
			for i := 0; i < tc.n; i++ {
				elapsed = acc.Add(tc.tick)
			}
			exact := new(big.Rat).SetFrac(
				new(big.Int).Mul(big.NewInt(int64(tc.n)*tc.tick), big.NewInt(tc.tb.Num)),
				big.NewInt(tc.tb.Den),
			)
			want := exact.FloatString(12)
			got := time.Duration(elapsed).Seconds()
			diff := new(big.Rat)
			diff.Sub(exact, big.NewRat(int64(elapsed), int64(time.Second)))
			absDiff := new(big.Rat).Abs(diff)
			limit := big.NewRat(1, 1000) // 1 ms
			if absDiff.Cmp(limit) >= 0 {
				t.Fatalf("accumulated %v, exact %s s, diff %s s exceeds 1ms", elapsed, want, absDiff.FloatString(12))
			}
			if got < 0 {
				t.Fatal("negative elapsed")
			}
		})
	}
}

func TestAccumulatorNoDriftAgainstSumOfParts(t *testing.T) {
	tb := Audio(48000)
	acc := NewAccumulator(tb)
	var direct int64
	for i := 0; i < 10000; i++ {
		acc.Add(960)
		direct += tb.Nanos(960)
	}
	// The carried residue means the running total is never more than one
	// nanosecond away from the sum of individually rounded values.
	diff := int64(acc.Elapsed()) - direct
	if diff < -1 || diff > 1 {
		t.Fatalf("accumulator drifted %d ns from per-packet sum", diff)
	}
}

func TestTicksFromDuration(t *testing.T) {
	tb := Audio(48000)
	if got := tb.Ticks(20 * time.Millisecond); got != 960 {
		t.Fatalf("20ms at 48kHz = %d ticks, want 960", got)
	}
	if got := tb.Ticks(time.Second); got != 48000 {
		t.Fatalf("1s at 48kHz = %d ticks, want 48000", got)
	}
}

func TestInvalidTimebase(t *testing.T) {
	if _, err := New(0, 1); err == nil {
		t.Fatal("expected error for zero numerator")
	}
	if _, err := New(1, 0); err == nil {
		t.Fatal("expected error for zero denominator")
	}
	var tb Timebase
	if tb.Valid() {
		t.Fatal("zero value timebase reported valid")
	}
	if tb.Nanos(10) != 0 || tb.Ticks(time.Second) != 0 {
		t.Fatal("invalid timebase produced nonzero conversion")
	}
}
