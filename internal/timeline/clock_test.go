package timeline

import (
	"math"
	"testing"
)

func TestNanosecondConversionAndOverflow(t *testing.T) {
	got, err := ToNanoseconds(90_000, 90_000)
	if err != nil || got != 1_000_000_000 {
		t.Fatalf("one second = %d, %v", got, err)
	}
	got, err = (&Timebase{Timescale: 48_000, Start: 1_000}).Normalize(49_000)
	if err != nil || got != 1_000_000_000 {
		t.Fatalf("non-zero origin normalization = %d, %v", got, err)
	}
	if _, err := ToNanoseconds(math.MaxInt64, 1); err == nil {
		t.Fatal("expected overflow rejection")
	}
	if _, err := ConvertTicks(1, 0, 1000); err == nil {
		t.Fatal("zero source timescale must fail")
	}
}

func TestSequenceWraparound(t *testing.T) {
	var s WrapState
	s.Bits = 16
	values := []uint64{65534, 65535, 0, 1}
	want := []int64{65534, 65535, 65536, 65537}
	for i, raw := range values {
		got, err := s.Extend(raw)
		if err != nil || got != want[i] {
			t.Fatalf("16 bit wrap[%d]=%d want %d err=%v", raw, got, want[i], err)
		}
	}
	var ts WrapState
	ts.Bits = 32
	got, _ := ts.Extend(math.MaxUint32 - 1)
	if got != math.MaxUint32-1 {
		t.Fatalf("initial ts = %d", got)
	}
	got, _ = ts.Extend(10)
	if want := int64(math.MaxUint32 + 11); got != want {
		t.Fatalf("32 bit timestamp wrap = %d want %d", got, want)
	}
	// A late reordered packet is represented below the high-water mark.
	late := Extend16(65537, 65535)
	if late != 65535 {
		t.Fatalf("late packet extension = %d", late)
	}
}

func TestDTSMonotonic(t *testing.T) {
	if err := CheckDTSMonotonic(7, []int64{0, 1, 1, 3}); err != nil {
		t.Fatal(err)
	}
	err := CheckDTSMonotonic(7, []int64{0, 2, 1})
	if err == nil || !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("expected monotonic error, got %v", err)
	}
}
