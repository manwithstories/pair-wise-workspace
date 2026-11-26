package clock

import (
	"errors"
	"math"
	"testing"
)

func TestRateToMaster(t *testing.T) {
	cases := []struct {
		name       string
		num, den   int64
		ticks      int64
		wantMaster int64
	}{
		{"48kHz audio 1s", 48000, 1, 48000, 90000},
		{"90kHz already master", 90000, 1, 90000, 90000},
		{"25fps video 1 frame", 25, 1, 1, 3600},
		{"30000/1001 frame", 30000, 1001, 1, 3003},
		{"24000/1001 two frames", 24000, 1001, 2, 7507},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRate(tc.num, tc.den)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.ToMaster(tc.ticks)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantMaster {
				t.Fatalf("ToMaster(%d)=%d want %d", tc.ticks, got, tc.wantMaster)
			}
		})
	}
}

func TestRateRoundTrip(t *testing.T) {
	r, _ := NewRate(24000, 1001)
	master, err := r.ToMaster(24000) // one nominal second
	if err != nil {
		t.Fatal(err)
	}
	back, err := r.FromMaster(master)
	if err != nil {
		t.Fatal(err)
	}
	// Allow at most one source tick of rounding loss over a full second.
	if diff := back - 24000; diff < -1 || diff > 0 {
		t.Fatalf("round trip drift too large: %d", diff)
	}
}

func TestConvert(t *testing.T) {
	from, _ := NewRate(48000, 1)
	to, _ := NewRate(90000, 1)
	got, err := Convert(48000, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got != 90000 {
		t.Fatalf("Convert 48k->90k: got %d want 90000", got)
	}
}

func TestOverflowRejected(t *testing.T) {
	r, _ := NewRate(25, 1)
	if _, err := r.ToMaster(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
	if _, err := mul64(math.MaxInt64, 2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("mul64 overflow not caught: %v", err)
	}
	if _, err := add64(math.MaxInt64, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("add64 overflow not caught: %v", err)
	}
	if _, err := NewRate(0, 1); !errors.Is(err, ErrBadRate) {
		t.Fatalf("bad rate not caught")
	}
	// 30 minutes at master rate must stay safely inside int64.
	thirtyMin, err := MsToMaster(30 * 60 * 1000)
	if err != nil || thirtyMin != 162_000_000 {
		t.Fatalf("30min ticks wrong: %d err=%v", thirtyMin, err)
	}
}

func TestMsHelpers(t *testing.T) {
	m, _ := MsToMaster(1000)
	if m != 90000 || MasterToMs(90000) != 1000 {
		t.Fatal("millisecond helpers inconsistent")
	}
}

func TestWrapExpanderMonotonic(t *testing.T) {
	const period int64 = 1000
	w, err := NewWrapExpander(1, period)
	if err != nil {
		t.Fatal(err)
	}
	raw := []int64{900, 950, 990, 10, 60, 995, 5, 50}
	var expanded []int64
	events := 0
	for off, p := range raw {
		full, ev, err := w.Expand(p, int64(off))
		if err != nil {
			t.Fatal(err)
		}
		if ev != nil {
			events++
			if ev.RawPTS != p || ev.WrapCount != events {
				t.Fatalf("bad event: %+v", ev)
			}
		}
		expanded = append(expanded, full)
	}
	want := []int64{900, 950, 990, 1010, 1060, 1995, 2005, 2050}
	for i := range want {
		if expanded[i] != want[i] {
			t.Fatalf("idx %d got %d want %d (series %v)", i, expanded[i], want[i], expanded)
		}
	}
	if events != 2 || w.Wraps() != 2 {
		t.Fatalf("want 2 wraps, got %d", events)
	}
	for i := 1; i < len(expanded); i++ {
		if expanded[i] < expanded[i-1] {
			t.Fatal("expanded series not monotonic")
		}
	}
}

func TestWrapExpanderDuplicateAndJitter(t *testing.T) {
	w, _ := NewWrapExpander(0, 1000)
	full, ev, _ := w.Expand(500, 0)
	if ev != nil || full != 500 {
		t.Fatal("first sample wrong")
	}
	// Exact duplicate: no wrap, same expanded value.
	full, ev, _ = w.Expand(500, 1)
	if ev != nil || full != 500 {
		t.Fatalf("duplicate mishandled: full=%d ev=%v", full, ev)
	}
	// Small backward jitter inside half-period: no wrap, monotonic hold.
	full, ev, _ = w.Expand(495, 2)
	if ev != nil || full != 500 {
		t.Fatalf("jitter mishandled: full=%d ev=%v", full, ev)
	}
	// Real wrap: forward first, then a backward jump beyond half the period.
	// Axis was held at 500 through the 495 jitter, so wrap lands at 1015.
	if _, _, err := w.Expand(800, 3); err != nil {
		t.Fatal(err)
	}
	full, ev, _ = w.Expand(10, 4)
	if ev == nil || full != 1015 {
		t.Fatalf("wrap missed: full=%d ev=%v", full, ev)
	}
}

func TestWrapExpanderValidation(t *testing.T) {
	if _, err := NewWrapExpander(0, 0); !errors.Is(err, ErrBadPeriod) {
		t.Fatalf("bad period not caught: %v", err)
	}
	w, _ := NewWrapExpander(0, 1000)
	if _, _, err := w.Expand(-1, 0); err == nil {
		t.Fatal("negative PTS must be rejected")
	}
}
