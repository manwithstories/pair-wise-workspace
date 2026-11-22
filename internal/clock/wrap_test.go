package clock

import (
	"testing"
	"time"
)

func TestUnwrapperHandlesWrap(t *testing.T) {
	rate := Rate{Num: 90000, Den: 1}
	u, err := NewUnwrapper32(rate)
	if err != nil {
		t.Fatal(err)
	}
	// Start near the 32-bit ceiling; a forward step crosses the boundary.
	base := uint32(RTPMod - 1000)
	first, e := u.Push(RawPoint{Raw32: base, RefTime: 0})
	if e != nil || first.Wrapped {
		t.Fatalf("first push: %+v err=%v", first, e)
	}
	second, e := u.Push(RawPoint{Raw32: base + 2000, RefTime: Time(time.Millisecond)})
	if e != nil {
		t.Fatal(e)
	}
	if !second.Wrapped {
		t.Fatal("expected wrap to be detected")
	}
	if second.Linear != int64(base)+2000 {
		t.Fatalf("unwrapped linear = %d, want %d", second.Linear, int64(base)+2000)
	}
}

func TestLatePacketDoesNotRewriteFrontier(t *testing.T) {
	rate := Rate{Num: 90000, Den: 1}
	u, _ := NewUnwrapper32(rate)
	if _, e := u.Push(RawPoint{Raw32: 1000}); e != nil {
		t.Fatal(e)
	}
	if _, e := u.Push(RawPoint{Raw32: 5000}); e != nil {
		t.Fatal(e)
	}
	late, e := u.Push(RawPoint{Raw32: 2000})
	if e != nil {
		t.Fatal(e)
	}
	if !late.Late {
		t.Fatal("out-of-order packet must be flagged Late")
	}
	if u.Frontier() != 5000 {
		t.Fatalf("frontier moved to %d, committed timeline must be immutable", u.Frontier())
	}
}

func TestDriftEstimator(t *testing.T) {
	// Sender runs exactly 120 ppm fast against the 90 kHz reference clock.
	rate := Rate{Num: 90000, Den: 1}
	d, err := NewDriftEstimator(rate)
	if err != nil {
		t.Fatal(err)
	}
	const ppm = 120.0
	for i := int64(0); i <= 30; i++ {
		// 1 s spacing: per-second tick rounding is at most one 90 kHz tick,
		// i.e. ~11 ppm of quantization, far below the asserted band.
		refNs := i * int64(time.Second)
		nominalTicks := float64(refNs) * 90000 / 1e9
		senderTicks := int64(nominalTicks * (1 + ppm/1e6))
		if err := d.Add(DriftPoint{Linear: senderTicks, RefTime: Time(refNs)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.SlopePPM()
	if err != nil {
		t.Fatal(err)
	}
	if got < 115 || got > 125 {
		t.Fatalf("drift ppm = %.3f, want ~120", got)
	}
	if !ExceedsThreshold(got, 100) || ExceedsThreshold(got, 200) {
		t.Fatal("threshold classification wrong")
	}
}

func TestDriftEstimatorRequiresSpan(t *testing.T) {
	d, _ := NewDriftEstimator(Rate{Num: 90000, Den: 1})
	if _, err := d.SlopePPM(); err == nil {
		t.Fatal("empty estimator must fail")
	}
	_ = d.Add(DriftPoint{Linear: 1, RefTime: 0})
	if _, err := d.SlopePPM(); err == nil {
		t.Fatal("single point cannot determine drift")
	}
}

func TestDriftWindowMarksRange(t *testing.T) {
	w, err := WindowFromPoint(Time(time.Second), 10*time.Millisecond, 120)
	if err != nil {
		t.Fatal(err)
	}
	if w.Start != Time(990*time.Millisecond) || w.End != Time(1010*time.Millisecond) {
		t.Fatalf("window = %+v", w)
	}
	if !w.Contains(Time(1000*time.Millisecond)) || w.Contains(w.End) {
		t.Fatal("window half-open semantics violated")
	}
}
