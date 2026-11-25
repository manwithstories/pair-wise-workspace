package timeline

import (
	"errors"
	"math"
	"testing"
)

func TestPTSExpanderWrapsAt33Bits(t *testing.T) {
	x := &PTSExpander{}
	start := PTSTicks - 4
	values := []uint64{start, PTSTicks - 1, 2, 5}
	want := []int64{int64(start), int64(PTSTicks - 1), int64(PTSTicks + 2), int64(PTSTicks + 5)}
	for i, v := range values {
		got, err := x.Expand(v)
		if err != nil {
			t.Fatalf("Expand(%d): %v", v, err)
		}
		if got != want[i] {
			t.Fatalf("case %d: got %d want %d", i, got, want[i])
		}
	}
}

func TestPTSExpanderRejectsMoreThan33Bits(t *testing.T) {
	x := &PTSExpander{}
	if _, err := x.Expand(PTSMask + 1); err == nil {
		t.Fatal("expected out-of-range PTS error")
	}
}

func TestTicksToNS90kHz(t *testing.T) {
	if got := TicksToNS(90000); got != NSPerSecond {
		t.Fatalf("got %d ns, want %d", got, NSPerSecond)
	}
}

func TestDriftCorrector(t *testing.T) {
	// A 120 ppm fast clock reports 1.000120 s; correction returns ~1 s.
	c, err := NewDriftCorrector(120)
	if err != nil {
		t.Fatal(err)
	}
	raw := int64(math.Round(float64(NSPerSecond) * 1.000120))
	got := c.Correct(raw)
	if diff := abs64(got - NSPerSecond); diff > 1000 {
		t.Fatalf("corrected=%d diff=%d ns, want <=1000", got, diff)
	}

	if _, err := NewDriftCorrector(501); err == nil {
		t.Fatal("expected drift above 500 ppm to fail")
	}
}

func TestBuildPreservesVariableDurations(t *testing.T) {
	idx := []Index{
		{Offset: 0, Length: 4, PTS33: 100, DurationNS: 33_333_333, Keyframe: true},
		{Offset: 4, Length: 4, PTS33: 4000, DurationNS: 20_833_333},
		{Offset: 8, Length: 4, PTS33: 5875, DurationNS: 41_666_667},
	}
	spans, err := Build(idx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range idx {
		if spans[i].DurationNS != idx[i].DurationNS {
			t.Fatalf("duration changed at %d: %d != %d", i, spans[i].DurationNS, idx[i].DurationNS)
		}
	}
	if spans[2].StartNS <= spans[1].StartNS {
		t.Fatalf("PTS not monotonic: %+v", spans)
	}
}

func TestErrorCarriesOffsetAndExitCode(t *testing.T) {
	base := New(KindUnrecoverable, "codec.av1", 7, 1234, "bad obu length")
	if base.Offset != 1234 || base.Index != 7 {
		t.Fatalf("missing context: %+v", base)
	}
	wrapped := Wrap(KindInternal, "pipeline", -1, -1, base)
	var te *Error
	if !errors.As(wrapped, &te) || te.Offset != 1234 {
		t.Fatalf("offset not preserved: %v", wrapped)
	}
	if code := ExitCode(wrapped); code != 3 {
		t.Fatalf("exit code=%d want 3", code)
	}
	if code := ExitCode(New(KindArgument, "x", -1, -1, "bad")); code != 2 {
		t.Fatalf("argument exit=%d want 2", code)
	}
	if code := ExitCode(New(KindStorage, "x", -1, -1, "bad")); code != 4 {
		t.Fatalf("storage exit=%d want 4", code)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
