package clock

import "testing"

func TestSampleRoundTrip(t *testing.T) {
	for _, rate := range []int{44100, 48000, 96000} {
		for _, n := range []int64{0, 1, 1000, 44100 * 10, 96000*10 - 1} {
			tk := FromSamples(n, rate)
			if got := ToSamples(tk, rate); got != n {
				t.Fatalf("rate=%d n=%d round trip got %d", rate, n, got)
			}
		}
	}
}

func TestCrossRateEquality(t *testing.T) {
	// The same instant expressed in different rates maps to the same tick.
	a := FromSamples(44100, 44100)
	b := FromSamples(48000, 48000)
	c := FromSamples(96000, 96000)
	if a != b || b != c {
		t.Fatalf("1s mismatch: %d %d %d", a, b, c)
	}
	if ToMillis(a) != 1000 {
		t.Fatalf("1s = %v ms", ToMillis(a))
	}
}

func TestSubSamplePrecision(t *testing.T) {
	// One 48kHz sample is ~0.0208ms; ticks must resolve well under 0.1ms.
	one := FromSamples(1, 48000)
	if ms := ToMillis(one); ms <= 0 || ms >= 0.1 {
		t.Fatalf("tick quantum too coarse: %v ms", ms)
	}
	if FromMillis(1.0) != Tick(Rate/1000) {
		t.Fatalf("FromMillis(1) = %d", FromMillis(1.0))
	}
}
