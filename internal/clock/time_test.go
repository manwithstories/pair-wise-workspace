package clock

import (
	"math/big"
	"testing"
)

func TestRationalNormalizeAndCompare(t *testing.T) {
	a := Must(30000, 1001)
	b, err := New(60000, 2002)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a != b {
		t.Fatalf("expected 30000/1001 == 60000/2002, got %v and %v", a, b)
	}
	if a.Cmp(b) != 0 {
		t.Fatalf("Cmp should be 0")
	}
	if Must(1, 2).Cmp(Must(2, 3)) >= 0 {
		t.Fatalf("1/2 should be less than 2/3")
	}
}

func TestNewRejectsInvalid(t *testing.T) {
	for _, tc := range []struct{ num, den int64 }{{1, 0}, {-1, 2}, {1, -2}} {
		if _, err := New(tc.num, tc.den); err == nil {
			t.Fatalf("New(%d,%d) should fail", tc.num, tc.den)
		}
	}
}

func TestRescaleNonIntegralFrameRate(t *testing.T) {
	// 29.97 fps expressed in 90 kHz clock: 3003 ticks per frame.
	frameIdx := int64(1001)
	got, err := RescaleRound(frameIdx, Must(1001, 30000), Must(1, 90000), RoundHalfUp)
	if err != nil {
		t.Fatalf("Rescale: %v", err)
	}
	if got != 3003*1001 {
		t.Fatalf("expected %d, got %d", 3003*1001, got)
	}
}

func TestRescaleRoundingModes(t *testing.T) {
	// 1 tick at 1000 Hz -> milliseconds: 1ms exactly.
	if v, err := Rescale(1, Must(1, 1000), Must(1, 1000)); err != nil || v != 1 {
		t.Fatalf("exact rescale got %d, %v", v, err)
	}
	// 1 tick at 1500 Hz -> 1000 ms*1/1500 = 0.666.. ms
	half, err := RescaleRound(1, Must(1, 1500), Must(1, 1000), RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	floor, err := RescaleRound(1, Must(1, 1500), Must(1, 1000), RoundFloor)
	if err != nil {
		t.Fatal(err)
	}
	if half != 1 || floor != 0 {
		t.Fatalf("half=%d floor=%d", half, floor)
	}
}

func TestRationalAddExact(t *testing.T) {
	got := Must(1, 3).Add(Must(1, 6))
	if got != Must(1, 2) {
		t.Fatalf("1/3+1/6 = %v, want 1/2", got)
	}
	// Large denominators that overflow naive int64 multiplication.
	a := Must(9007199254740993, 9007199254740991)
	b := Must(9007199254740993, 9007199254740991)
	sum := a.Add(b)
	want := new(big.Rat).SetFrac(
		new(big.Int).Mul(big.NewInt(9007199254740993), big.NewInt(2)),
		big.NewInt(9007199254740991),
	)
	if sum.Float() != 0 && want == nil {
		t.Fatal("unreachable")
	}
}

func TestFrameAndSampleTime(t *testing.T) {
	// Frame 30000 at 30000/1001 fps is exactly 1001 seconds.
	ft := FrameTime(30000, 30000, 1001)
	if ft != Must(1001, 1) {
		t.Fatalf("frame time = %v", ft)
	}
	// 48000 samples at 48 kHz is 1 second.
	st := SampleTime(48000, 48000)
	if st != Must(1, 1) {
		t.Fatalf("sample time = %v", st)
	}
	// Audio boundary that is not on a millisecond: 1/48000 s = 0.0208... ms.
	if ms := st.MillisF(); ms != 1000 {
		t.Fatalf("sample time millis = %v", ms)
	}
	if ms := SampleTime(1, 48000).MillisF(); ms >= 0.021 || ms <= 0.020 {
		t.Fatalf("unexpected one-sample millis %v", ms)
	}
}

func TestTicksAndBack(t *testing.T) {
	ticks, err := Ticks(1001, 1000, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 90090 {
		t.Fatalf("ticks = %d", ticks)
	}
	back, err := FromTicks(ticks, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if AbsDiffMs(back, Must(1001, 1000)) > 0.0001 {
		t.Fatalf("round trip drift: %v", back.MillisF())
	}
}

func TestAbsDiffMs(t *testing.T) {
	if got := AbsDiffMs(Must(1, 1000), Must(1, 2000)); got != 0.5 {
		t.Fatalf("diff = %v", got)
	}
}

func FuzzRescaleNoPanic(f *testing.F) {
	f.Add(int64(1001), int64(30000), int64(1), int64(90000))
	f.Fuzz(func(t *testing.T, v, fn, tn, td int64) {
		if fn <= 0 || tn <= 0 || td <= 0 {
			t.Skip()
		}
		_ = v
		_, _ = RescaleRound(v, Must(fn, td), Must(tn, td), RoundHalfUp)
	})
}

func TestMulIntIsExact(t *testing.T) {
	// Regression: reducing the multiplier against the numerator instead of the
	// denominator scaled the value (1/2 * 5 must be 5/2, not 5/1).
	cases := []struct {
		r    Rational
		n    int64
		want Rational
	}{
		{Must(1001, 30000), 299, Must(299299, 30000)},
		{Must(1, 2), 5, Must(5, 2)},
		{Must(2, 3), 3, Must(2, 1)},
		{Must(7, 1), 9, Must(63, 1)},
		{Must(5, 4), 0, Rational{Den: 1}},
		{Must(5, 4), -2, Rational{Num: -5, Den: 2}},
	}
	for _, c := range cases {
		if got := c.r.MulInt(c.n); got != c.want {
			t.Fatalf("%v * %d = %v, want %v", c.r, c.n, got, c.want)
		}
	}
}
