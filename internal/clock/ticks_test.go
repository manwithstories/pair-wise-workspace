package clock

import (
	"errors"
	"testing"

	"medialine/internal/fault"
)

func TestTicksToMicrosRoundTrip(t *testing.T) {
	cases := []struct {
		ticks     int64
		timescale uint64
		want      int64
	}{
		{0, 90000, 0},
		{90000, 90000, 1_000_000},
		{-45000, 90000, -500_000},
		{1, 90000, 11},
		{48_000, 48_000, 1_000_000},
		{2_000_000, 90_000, 22_222_222},
	}
	for _, tc := range cases {
		got, err := TicksToMicros(tc.ticks, tc.timescale)
		if err != nil {
			t.Fatalf("TicksToMicros(%d,%d): %v", tc.ticks, tc.timescale, err)
		}
		if got != tc.want {
			t.Errorf("TicksToMicros(%d,%d)=%d want %d", tc.ticks, tc.timescale, got, tc.want)
		}
	}
}

func TestTicksToMicrosErrors(t *testing.T) {
	if _, err := TicksToMicros(1, 0); !errors.Is(err, ErrTimescale) {
		t.Fatalf("expected ErrTimescale, got %v", err)
	}
	if _, err := TicksToMicros(1<<62, 1); err == nil {
		t.Fatal("expected overflow")
	}
	if _, err := mulDiv(1<<62, 1<<10, 1); err == nil {
		t.Fatal("expected overflow from mulDiv")
	}
	if _, err := mulDiv(-1<<62, 1<<10, 1); err == nil {
		t.Fatal("expected negative overflow from mulDiv")
	}
	if got, err := mulDiv(-10, 3, 1); err != nil || got != -30 {
		t.Fatalf("mulDiv(-10,3,1)=%d,%v", got, err)
	}
}

func TestMicrosToTicks(t *testing.T) {
	got, err := MicrosToTicks(1_000_000, 90_000)
	if err != nil || got != 90_000 {
		t.Fatalf("MicrosToTicks=%d,%v", got, err)
	}
	if _, err := MicrosToTicks(1, 0); !errors.Is(err, ErrTimescale) {
		t.Fatalf("expected ErrTimescale")
	}
}

func TestUnwrapperWrap(t *testing.T) {
	u, err := NewUnwrapper(32)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []uint64{4294967290, 4294967295, 0, 5}
	want := []int64{4294967290, 4294967295, 4294967296, 4294967301}
	for i, in := range inputs {
		got, wrapped, err := u.Unwrap(in)
		if err != nil {
			t.Fatalf("input %d: %v", in, err)
		}
		if got != want[i] {
			t.Errorf("Unwrap(%d)=%d want %d", in, got, want[i])
		}
		if wrapped != (i == 2) {
			t.Errorf("Unwrap(%d) wrapped=%v", in, wrapped)
		}
	}
}

func TestUnwrapperRejectsBackwards(t *testing.T) {
	u, _ := NewUnwrapper(32)
	if _, _, err := u.Unwrap(1000); err != nil {
		t.Fatal(err)
	}
	got, wrapped, err := u.Unwrap(900)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped {
		t.Fatal("small backwards step should not wrap")
	}
	if got != 900 {
		t.Fatalf("got %d", got)
	}
}

func TestUnwrapperBadWidth(t *testing.T) {
	if _, err := NewUnwrapper(33); err == nil {
		t.Fatal("expected error for 33-bit width")
	}
}

func TestConverterNormalisesNonZeroStart(t *testing.T) {
	c, err := New(Options{Timescale: 90_000, WrapBits: 0})
	if err != nil {
		t.Fatal(err)
	}
	// First timestamp is 30s in; result should still start at 0us.
	got, err := c.Convert(30 * 90_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("first sample=%d want 0", got)
	}
	got, _ = c.Convert(31 * 90_000)
	if got != 1_000_000 {
		t.Fatalf("second sample=%d want 1000000", got)
	}
}

func TestConverterNegativeStart(t *testing.T) {
	c, _ := New(Options{Timescale: 1000, StartUS: -500})
	got, err := c.Convert(0)
	if err != nil {
		t.Fatal(err)
	}
	if got != -500 {
		t.Fatalf("got %d want -500", got)
	}
}

func TestConverterDetectsDuplicatesAndBackwards(t *testing.T) {
	c, _ := New(Options{Timescale: 1000})
	if _, err := c.Convert(0); err != nil {
		t.Fatal(err)
	}
	_, err := c.Convert(0)
	if fe, ok := fault.As(err); !ok || fe.Kind != fault.KindDuplicateTime {
		t.Fatalf("expected duplicate fault, got %v", err)
	}
	c2, _ := New(Options{Timescale: 1000})
	c2.Convert(1000)
	_, err = c2.Convert(500)
	if fe, ok := fault.As(err); !ok || fe.Kind != fault.KindBackwardsTime {
		t.Fatalf("expected backwards fault, got %v", err)
	}
}

func TestConverterAllowsDuplicates(t *testing.T) {
	c, _ := New(Options{Timescale: 1000, AllowDuplicates: true})
	c.Convert(10)
	if _, err := c.Convert(10); err != nil {
		t.Fatalf("duplicate should be allowed: %v", err)
	}
	if c.Samples() != 2 {
		t.Fatalf("samples=%d", c.Samples())
	}
}

func TestConverterZeroTimescale(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error for zero timescale")
	}
}
