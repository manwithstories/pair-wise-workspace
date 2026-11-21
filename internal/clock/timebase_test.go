package clock

import (
	"testing"
	"time"
)

func TestNewReducesAndValidates(t *testing.T) {
	tb, err := New(2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Num != 1 || tb.Den != 2 {
		t.Fatalf("got %+v", tb)
	}
	if _, err := New(0, 1); err == nil {
		t.Fatal("expected invalid timebase")
	}
	if _, err := New(1, -2); err == nil {
		t.Fatal("expected invalid timebase")
	}
}

func TestConvertIsExactAndRounds(t *testing.T) {
	ms := Millisecond
	v90k := Video90k
	if got := ms.TicksForMillis(40); got != 40 {
		t.Fatalf("40ms in ms ticks = %d", got)
	}
	if got := v90k.TicksForMillis(40); got != 3600 {
		t.Fatalf("40ms in 90k = %d", got)
	}
	// 1 tick at 90k is 1/90000 s == 0.0111 ms; rounds to 0.
	if got := v90k.ToMillis(1); got != 0 {
		t.Fatalf("1@90k to ms = %d", got)
	}
	// 90 ticks at 90k is exactly 1 ms.
	if got := v90k.ToMillis(90); got != 1 {
		t.Fatalf("90@90k to ms = %d", got)
	}
	// 33333 ticks at 90k = 370.366.. ms -> 370.
	if got := v90k.ToMillis(33333); got != 370 {
		t.Fatalf("got %d", got)
	}
	back, err := v90k.Convert(3600, ms)
	if err != nil || back != 40 {
		t.Fatalf("convert 3600@90k -> ms = %d, %v", back, err)
	}
}

func TestNoFloatAccumulation(t *testing.T) {
	// 3000 increments of 1/90000s must land exactly on 1/30s worth of ticks.
	v := Video90k
	acc := int64(0)
	for i := 0; i < 3000; i++ {
		acc = v.Add(acc, 1)
	}
	if got := v.ToMillis(acc); got != 33 {
		t.Fatalf("3000 ticks @90k should be 33ms, got %d", got)
	}
	if acc != 3000 {
		t.Fatalf("accumulated %d", acc)
	}
}

func TestDurationConversion(t *testing.T) {
	if got := Video90k.TicksForDuration(40 * time.Millisecond); got != 3600 {
		t.Fatalf("got %d", got)
	}
}

func TestCeilDiv(t *testing.T) {
	cases := []struct{ a, b, want int64 }{{0, 4, 0}, {1, 4, 1}, {4, 4, 1}, {5, 4, 2}, {-1, 4, 0}}
	for _, c := range cases {
		if got := CeilDiv(c.a, c.b); got != c.want {
			t.Fatalf("CeilDiv(%d,%d)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}
