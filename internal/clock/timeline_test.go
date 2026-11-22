package clock

import (
	"errors"
	"testing"
)

func TestScale(t *testing.T) {
	ns, err := Scale(90000, 90000)
	if err != nil || ns != NsPerSec {
		t.Fatalf("Scale(90000,90000) = %d, %v; want 1e9, nil", ns, err)
	}
	if _, err := Scale(-1, 90000); err == nil {
		t.Fatal("negative duration not reported")
	}
	if _, err := Scale(1, 0); err == nil {
		t.Fatal("zero rate not reported")
	}
	if _, err := Scale(1<<62, 90000); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow: got %v", err)
	}
}

func TestDurationNegative(t *testing.T) {
	if _, err := Duration(Time(10), Time(5)); err == nil {
		t.Fatal("negative duration accepted")
	}
	if d, _ := Duration(Time(5), Time(10)); d != 5 {
		t.Fatalf("Duration = %d, want 5", d)
	}
}

func TestWrapTrackerForwardWrap(t *testing.T) {
	w := NewWrapTracker(90000)
	base, err := w.Convert(0xFFFFFFF0)
	if err != nil {
		t.Fatal(err)
	}
	after, err := w.Convert(0x10) // wrapped past 2^32
	if err != nil {
		t.Fatal(err)
	}
	if after <= base {
		t.Fatalf("wrapped ts not extended: base=%d after=%d", base, after)
	}
	want, _ := Scale(1<<32+0x10, 90000)
	if after != want {
		t.Fatalf("after=%d want %d", after, want)
	}
}

func TestWrapTrackerLatePreWrapPacket(t *testing.T) {
	w := NewWrapTracker(90000)
	if _, err := w.Convert(0xFFFFFFF0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Convert(0x20); err != nil { // wrap
		t.Fatal(err)
	}
	late, err := w.Convert(0xFFFFFF00) // belongs to previous epoch
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Scale(0xFFFFFF00, 90000)
	if late != want {
		t.Fatalf("late=%d want %d", late, want)
	}
}

func TestWrapTrackerNonMonotonic(t *testing.T) {
	w := NewWrapTracker(90000)
	if _, err := w.Convert(1 << 24); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Convert(1 << 20); err == nil {
		t.Fatal("large backward jump not reported")
	}
}

func TestWrapTrackerSnapshotRestore(t *testing.T) {
	w := NewWrapTracker(90000)
	if _, err := w.Convert(0xFFFFFFF0); err != nil {
		t.Fatal(err)
	}
	e, l, le, i := w.Snapshot()
	w2 := NewWrapTracker(90000)
	w2.Restore(e, l, le, i)
	a, err1 := w.Convert(0x10)
	b, err2 := w2.Convert(0x10)
	if err1 != nil || err2 != nil || a != b {
		t.Fatalf("restore mismatch: %d/%v vs %d/%v", a, err1, b, err2)
	}
}
