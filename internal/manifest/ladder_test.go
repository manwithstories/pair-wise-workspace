package manifest

import (
	"bytes"
	"testing"
)

func TestAlignSnapsToCanonical(t *testing.T) {
	reps := []Rep{
		{Bitrate: 800, Boundaries: []int64{180, 360, 540}},
		{Bitrate: 1600, Boundaries: []int64{181, 360, 540}}, // 1-frame jitter
		{Bitrate: 3200, Boundaries: []int64{180, 359, 540}},
	}
	l, err := Align("30000/1001", reps, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Reps {
		for i, b := range r.Boundaries {
			if b != l.Canonical[i] {
				t.Fatalf("rep %d boundary %d = %d, want canonical %d",
					r.Bitrate, i, b, l.Canonical[i])
			}
		}
	}
}

func TestAlignRejectsExcessDeviation(t *testing.T) {
	reps := []Rep{
		{Bitrate: 800, Boundaries: []int64{180, 360}},
		{Bitrate: 1600, Boundaries: []int64{182, 360}},
	}
	if _, err := Align("30", reps, 1); err == nil {
		t.Fatal("expected deviation error")
	}
}

func TestAlignRejectsNonMonotonic(t *testing.T) {
	reps := []Rep{
		{Bitrate: 800, Boundaries: []int64{360, 180}},
		{Bitrate: 1600, Boundaries: []int64{360, 180}},
	}
	if _, err := Align("30", reps, 1); err == nil {
		t.Fatal("expected monotonic error")
	}
}

func TestAlignRejectsCountMismatch(t *testing.T) {
	reps := []Rep{
		{Bitrate: 800, Boundaries: []int64{180, 360}},
		{Bitrate: 1600, Boundaries: []int64{180}},
	}
	if _, err := Align("30", reps, 1); err == nil {
		t.Fatal("expected count mismatch error")
	}
}

func TestMarshalInitStyp(t *testing.T) {
	b, err := MarshalInit()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 8 || !bytes.Contains(b, []byte("styp")) || !bytes.Contains(b, []byte("msdh")) {
		t.Fatalf("bad styp box: %q", b)
	}
}
