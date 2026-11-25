package clock

import (
	"math"
	"testing"
)

func TestTicksToNsRational(t *testing.T) {
	got, err := TicksToNs(48000, 1, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if got != NsPerSecond {
		t.Fatalf("48000 samples = %d ns, want %d", got, NsPerSecond)
	}

	got, err = TicksToNs(90000, 1, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if got != NsPerSecond {
		t.Fatalf("90k RTP ticks = %d ns", got)
	}

	// 30 fps, 1000 timescale ticks per frame.
	got, err = TicksToNs(15000, 1, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 500*NsPerMs {
		t.Fatalf("15000/30000 = %d, want 500ms", got)
	}
}

func TestTicksToNsOverflowAndBadTimeBase(t *testing.T) {
	if _, err := TicksToNs(math.MaxInt64, 1, 1); err == nil {
		t.Fatal("expected overflow")
	} else if ce, ok := AsError(err); !ok || ce.Code != CodeOverflow {
		t.Fatalf("expected structured overflow, got %v", err)
	}

	if _, err := TicksToNs(1, 0, 1); err == nil {
		t.Fatal("expected invalid time base")
	}
}

func TestIntervalHalfOpenAndNegative(t *testing.T) {
	good := Interval{Start: 10, End: 20}
	if err := good.Valid(); err != nil {
		t.Fatal(err)
	}
	if good.Duration() != 10 || !good.Contains(19) || good.Contains(20) {
		t.Fatal("half-open interval semantics are wrong")
	}
	if err := (Interval{Start: 20, End: 10}).Valid(); err == nil {
		t.Fatal("expected reversed interval error")
	} else if ce, ok := AsError(err); !ok || ce.Code != CodeBadInterval {
		t.Fatalf("expected bad_interval, got %v", err)
	}
	if err := (Interval{Start: -1, End: 10}).Valid(); err == nil {
		t.Fatal("expected negative error")
	}
}

func TestRoundToSample(t *testing.T) {
	idx, rounded, err := RoundToSample(20_833, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 || rounded != 20_833 {
		t.Fatalf("nearest sample: idx=%d rounded=%d", idx, rounded)
	}
	if _, _, err := RoundToSample(-1, 48000); err == nil {
		t.Fatal("negative time must not round")
	}
}
