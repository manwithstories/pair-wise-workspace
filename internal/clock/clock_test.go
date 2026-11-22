package clock

import "testing"

func TestTicksMillisRoundTrip(t *testing.T) {
	for _, ms := range []uint64{0, 1, 33, 1000, 90000} {
		got := TicksToMillis(MillisToTicks(float64(ms)))
		if got != ms {
			t.Fatalf("ms=%d round trip got %d", ms, got)
		}
	}
}

func TestSamplesToTicks(t *testing.T) {
	// 2 s of 48 kHz audio is exactly 180000 ticks.
	if got := SamplesToTicks(96000, Rate48000); got != 180000 {
		t.Fatalf("48k: got %d want 180000", got)
	}
	// 2 s of 44.1 kHz audio is also exactly 180000 ticks.
	if got := SamplesToTicks(88200, Rate44100); got != 180000 {
		t.Fatalf("44.1k: got %d want 180000", got)
	}
	if got := TicksToSamples(180000, Rate48000); got != 96000 {
		t.Fatalf("ticks->samples: got %d want 96000", got)
	}
}

func TestRepairerMonotonic(t *testing.T) {
	r := NewRepairer(TicksPerFrame30)
	for i := 0; i < 5; i++ {
		start, st, rep := r.Add(i, uint64(i)*180000, 180000)
		if st != StatusOK || rep != nil {
			t.Fatalf("seg %d: status %v", i, st)
		}
		if start != uint64(i)*180000 {
			t.Fatalf("seg %d: start %d", i, start)
		}
	}
}

func TestRepairerRegression(t *testing.T) {
	r := NewRepairer(TicksPerFrame30)
	r.Add(0, 0, 180000)
	r.Add(1, 180000, 180000)
	// Encoder restart: segment 2 claims to start at 0 again.
	start, st, rep := r.Add(2, 0, 180000)
	if st != StatusRepaired || rep == nil {
		t.Fatalf("status %v want repaired", st)
	}
	if start != 360000 {
		t.Fatalf("adjusted start %d want 360000", start)
	}
	// Deterministic: replaying the same sequence yields the same map.
	r2 := NewRepairer(TicksPerFrame30)
	r2.Add(0, 0, 180000)
	r2.Add(1, 180000, 180000)
	start2, _, _ := r2.Add(2, 0, 180000)
	if start2 != start {
		t.Fatalf("non-deterministic repair: %d vs %d", start2, start)
	}
}

func TestRepairerWholeSegmentGap(t *testing.T) {
	r := NewRepairer(TicksPerFrame30)
	r.Add(0, 0, 180000)
	// Segment 1 missing entirely; segment 2 starts exactly 2 durations out.
	_, st, rep := r.Add(2, 540000, 180000)
	if st != StatusRepaired || rep == nil {
		t.Fatalf("status %v want repaired", st)
	}
}

func TestRepairerAmbiguousGapUnrecoverable(t *testing.T) {
	r := NewRepairer(TicksPerFrame30)
	r.Add(0, 0, 180000)
	// Gap of 1.5 segments cannot be resolved deterministically.
	_, st, rep := r.Add(1, 180000+270000, 180000)
	if st != StatusUnrecoverable || rep == nil {
		t.Fatalf("status %v want unrecoverable", st)
	}
	// Timeline resynchronises on the observed start afterwards.
	_, st, _ = r.Add(2, 450000+180000, 180000)
	if st != StatusOK {
		t.Fatalf("resync status %v want ok", st)
	}
}

func TestUnwrap33(t *testing.T) {
	const w = uint64(1) << 33
	// No wrap.
	if got := Unwrap33(1000, 1000+180000); got != 1000+180000 {
		t.Fatalf("no-wrap: got %d", got)
	}
	// Wrapped forward across 2^33.
	ref := w - 90000
	raw := uint64(90000) // ref + 180000 mod 2^33
	if got := Unwrap33(ref, raw); got != w+90000 {
		t.Fatalf("wrap: got %d want %d", got, w+90000)
	}
	// Wrapped backward.
	if got := Unwrap33(90000, w-90000); got != ^uint64(90000 - 1) { // -90000 as uint64
		t.Fatalf("backward wrap: got %d", got)
	}
}

func TestErrorFormat(t *testing.T) {
	e := &Error{Op: "align", Segment: 7, StartTick: 0, EndTick: 180000, Msg: "gap"}
	s := e.Error()
	for _, want := range []string{"segment=7", "align", "gap"} {
		if !contains(s, want) {
			t.Fatalf("error %q missing %q", s, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
