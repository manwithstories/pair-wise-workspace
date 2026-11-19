package clock

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func TestRationalArithmeticIsExact(t *testing.T) {
	a := NewRational(1, 3)
	b := NewRational(1, 6)
	sum := a.Add(b)
	if sum != NewRational(1, 2) {
		t.Fatalf("1/3+1/6 = %s, want 1/2", sum)
	}
	// 1001/30000 accumulated 30000 times must be exactly 1001 seconds.
	step := NewRational(1001, 30000)
	acc := Zero()
	for i := 0; i < 30000; i++ {
		acc = acc.Add(step)
	}
	if acc != NewRational(1001, 1) {
		t.Fatalf("accumulated frame duration = %s, want 1001/1", acc)
	}
}

func TestBaseTimeAndTicks(t *testing.T) {
	base := NewBase(1, 90000)
	got := base.Time(90000)
	if got != NewRational(1, 1) {
		t.Fatalf("90000 ticks = %s, want 1/1", got)
	}
	if ticks := base.Ticks(NewRational(3, 2)); ticks != 135000 {
		t.Fatalf("1.5s in 90k = %d, want 135000", ticks)
	}
}

func TestMillisRoundsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		r    Rational
		want int64
	}{
		{NewRational(1, 2000), 1},
		{NewRational(-1, 2000), -1},
		{NewRational(498, 2000), 249},
		{NewRational(499, 2000), 250},
		{NewRational(3, 2), 1500},
	}
	for _, c := range cases {
		if got := c.r.Millis(); got != c.want {
			t.Errorf("%s.Millis() = %d, want %d", c.r, got, c.want)
		}
	}
}

func TestSourceErrorKeepsOffset(t *testing.T) {
	base := errors.New("syntax")
	err := NewSourceError("srt", "bad_timestamp", 128, "unparseable")
	err.Err = base
	var se *SourceError
	if !errors.As(err, &se) {
		t.Fatal("errors.As failed to find SourceError")
	}
	if se.Offset != 128 || se.Code != "bad_timestamp" {
		t.Fatalf("unexpected error fields: %+v", se)
	}
	if !errors.Is(err, base) {
		t.Fatal("wrapped cause not reachable")
	}
	found, ok := AsSourceError(err)
	if !ok || found.Offset != 128 {
		t.Fatalf("AsSourceError = %+v, %v", found, ok)
	}
}

func TestUnwrapperHandlesWrapDuplicateAndBackstep(t *testing.T) {
	base := uint32(4294967000)
	u := NewUnwrapper(90000 * 10)

	t1, err := u.Push(rtp.Header{SequenceNumber: 1, Timestamp: base})
	if err != nil {
		t.Fatal(err)
	}
	if t1.Extended != int64(base) {
		t.Fatalf("first extended = %d", t1.Extended)
	}

	// 3000 ticks later crosses 2^32.
	t2, _ := u.Push(rtp.Header{SequenceNumber: 2, Timestamp: base + 3000})
	if !t2.Wrap {
		t.Fatal("expected wrap flag")
	}
	if t2.Extended-t1.Extended != 3000 {
		t.Fatalf("wrap advance = %d, want 3000", t2.Extended-t1.Extended)
	}
	if u.Wraps() != 1 {
		t.Fatalf("wraps = %d, want 1", u.Wraps())
	}

	// Duplicate timestamp must be preserved, not repaired.
	t3, _ := u.Push(rtp.Header{SequenceNumber: 3, Timestamp: base + 3000})
	if !t3.Duplicate || t3.Extended != t2.Extended || t3.Raw != base+3000 {
		t.Fatalf("duplicate handling wrong: %+v", t3)
	}

	// A backwards step keeps the raw value.
	t4, _ := u.Push(rtp.Header{SequenceNumber: 4, Timestamp: base + 1000})
	if !t4.Backward || t4.Raw != base+1000 {
		t.Fatalf("backward handling wrong: %+v", t4)
	}

	// A huge jump is flagged but the value survives.
	t5, _ := u.Push(rtp.Header{SequenceNumber: 5, Timestamp: base + 3000 + 90000*60})
	if !t5.Jump {
		t.Fatalf("expected jump flag, got %+v", t5)
	}
	if t5.Raw != base+3000+90000*60 {
		t.Fatalf("jump lost raw timestamp: %d", t5.Raw)
	}
}

func TestUnwrapperFlagsSequenceDisorder(t *testing.T) {
	u := NewUnwrapper(0)
	u.Push(rtp.Header{SequenceNumber: 10, Timestamp: 1000})
	got, _ := u.Push(rtp.Header{SequenceNumber: 8, Timestamp: 4000})
	if !got.Disorder {
		t.Fatalf("expected disorder flag, got %+v", got)
	}
}

func TestUnwrapperUnderflowBeforeWrap(t *testing.T) {
	u := NewUnwrapper(0)
	u.Push(rtp.Header{SequenceNumber: 1, Timestamp: 100})
	if _, err := u.Push(rtp.Header{SequenceNumber: 2, Timestamp: 0xFFFF0000}); err == nil {
		t.Fatal("expected underflow error")
	}
}

func TestUnwrapRTPSimple(t *testing.T) {
	var u UnwrapRTP
	vals := []uint32{MaxRTPTimestamp - 10, MaxRTPTimestamp, 5, 100}
	var out []int64
	for _, v := range vals {
		out = append(out, u.Push(v))
	}
	for i := 1; i < len(out); i++ {
		if out[i] <= out[i-1] {
			t.Fatalf("not monotonic at %d: %v", i, out)
		}
	}
	if out[len(out)-1] != int64(MaxRTPTimestamp)+1+100 {
		t.Fatalf("final = %d", out[len(out)-1])
	}
}

func TestEstimateRecoversDrift(t *testing.T) {
	base := NewBase(1, 90000)
	// Node clock runs 85 ppm fast and starts 2 seconds late.
	pairs := make([]Pair, 0, 200)
	for i := int64(0); i < 200; i++ {
		src := i * 90000 // one second apart
		node := src + 2*90000 + src*85/1_000_000
		pairs = append(pairs, Pair{SourceTicks: src, NodeTicks: node})
	}
	corr, anomalies, ok := Estimate(pairs, base, NewRational(1, 100))
	if !ok {
		t.Fatal("estimate failed")
	}
	if len(anomalies) != 0 {
		t.Fatalf("unexpected anomalies: %+v", anomalies)
	}
	drift := corr.DriftPPM.Seconds()
	if drift < 80 || drift > 90 {
		t.Fatalf("drift = %.2f ppm, want ~85", drift)
	}
	off := corr.Offset.Seconds()
	if off < 1.9 || off > 2.1 {
		t.Fatalf("offset = %.4f s, want ~2", off)
	}
	// Correcting the last node value should land near its source time.
	corrected := corr.Correct(pairs[199].NodeTicks, base)
	want := base.Time(pairs[199].SourceTicks)
	diff := corrected.Sub(want)
	if diff.Seconds() > 0.01 || diff.Seconds() < -0.01 {
		t.Fatalf("corrected %s want %s", corrected, want)
	}
}

func TestEstimateFlagsJumpAnomaly(t *testing.T) {
	base := NewBase(1, 90000)
	pairs := make([]Pair, 0, 50)
	for i := int64(0); i < 50; i++ {
		src := i * 90000
		node := src + 5*90000
		if i == 30 {
			node += 30 * 90000 // 30 second jump
		}
		pairs = append(pairs, Pair{SourceTicks: src, NodeTicks: node})
	}
	corr, anomalies, _ := Estimate(pairs, base, NewRational(1, 1))
	if corr.Samples != 50 {
		t.Fatalf("samples = %d", corr.Samples)
	}
	if len(anomalies) == 0 {
		t.Fatal("expected jump anomaly")
	}
	found := false
	for _, a := range anomalies {
		if a.Index == 30 && a.NodeTicks == pairs[30].NodeTicks {
			found = true
		}
	}
	if !found {
		t.Fatalf("jump sample not reported with raw value: %+v", anomalies)
	}
}

func TestEstimateRefitsAroundJump(t *testing.T) {
	base := NewBase(1, 90000)
	pairs := make([]Pair, 0, 300)
	for i := int64(0); i < 300; i++ {
		src := i * 90000
		node := src + 2*90000 + src*85/1_000_000
		if i == 150 || i == 151 {
			node += 45 * 90000 // two consecutive jumped timestamps
		}
		pairs = append(pairs, Pair{SourceTicks: src, NodeTicks: node})
	}
	corr, anomalies, ok := Estimate(pairs, base, NewRational(1, 100))
	if !ok {
		t.Fatal("estimate failed")
	}
	if len(anomalies) != 2 {
		t.Fatalf("anomalies = %+v, want 2", anomalies)
	}
	drift := corr.DriftPPM.Seconds()
	if drift < 80 || drift > 90 {
		t.Fatalf("drift = %.2f ppm, want ~85 after refit", drift)
	}
}
