package timebase

import (
	"testing"

	"archive/internal/media"
)

func TestFitIdentity(t *testing.T) {
	c, err := Fit([]media.Anchor{{Local: 0, Ref: 0}, {Local: 1_000_000_000, Ref: 1_000_000_000}})
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if got := c.Apply(500_000_000); got != 500_000_000 {
		t.Fatalf("identity apply = %d", got)
	}
	if ppm := c.DriftPPM(); ppm != 0 {
		t.Fatalf("drift = %d ppm", ppm)
	}
}

func TestFitDriftWithinLimit(t *testing.T) {
	// Local clock runs 300 ppm fast: local = ref * 1.0003.
	anchors := []media.Anchor{
		{Local: 0, Ref: 0},
		{Local: 600_180_000, Ref: 600_000_000},
		{Local: 1_200_360_000, Ref: 1_200_000_000},
		{Local: 3_000_900_000, Ref: 3_000_000_000},
	}
	c, err := Fit(anchors)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if ppm := c.DriftPPM(); ppm != -300 {
		t.Fatalf("drift = %d ppm, want -300", ppm)
	}
	// Residual at check anchors must stay far below one 48 kHz sample
	// (20833 ns).
	for _, a := range anchors[1:3] {
		if got := c.Apply(a.Local); got != a.Ref {
			d := got - a.Ref
			if d < 0 {
				d = -d
			}
			if d > 100 {
				t.Fatalf("residual %d ns at ref %d", d, a.Ref)
			}
		}
	}
}

func TestFitRejectsExcessiveDrift(t *testing.T) {
	anchors := []media.Anchor{
		{Local: 0, Ref: 0},
		{Local: 1_000_000_000, Ref: 999_000_000}, // -1000 ppm
	}
	_, err := Fit(anchors)
	if err == nil {
		t.Fatal("expected fault for drift beyond ±500 ppm")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Stage != media.StageCalibrate {
		t.Fatalf("want calibrate fault, got %v", err)
	}
}

func TestFitNeedsTwoAnchors(t *testing.T) {
	if _, err := Fit([]media.Anchor{{Local: 0, Ref: 0}}); err == nil {
		t.Fatal("expected error for single anchor")
	}
}

func TestUnwrap33NoWrap(t *testing.T) {
	got, err := Unwrap33([]int64{100, 200, 300}, 1)
	if err != nil {
		t.Fatalf("Unwrap33: %v", err)
	}
	for i, want := range []int64{100, 200, 300} {
		if got[i] != want {
			t.Fatalf("out[%d]=%d want %d", i, got[i], want)
		}
	}
}

func TestUnwrap33AcrossWrap(t *testing.T) {
	const m = int64(1) << 33
	raw := []int64{m - 10, m - 5, 3, 8}
	got, err := Unwrap33(raw, 1)
	if err != nil {
		t.Fatalf("Unwrap33: %v", err)
	}
	want := []int64{m - 10, m - 5, m + 3, m + 8}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("out[%d]=%d want %d", i, got[i], want[i])
		}
	}
}

func TestUnwrap33BackwardStep(t *testing.T) {
	// Slight reordering inside half the modulus unwraps backwards.
	got, err := Unwrap33([]int64{1000, 900, 1100}, 1)
	if err != nil {
		t.Fatalf("Unwrap33: %v", err)
	}
	if got[1] != 900 || got[2] != 1100 {
		t.Fatalf("got %v", got)
	}
}

func TestUnwrap33Ambiguous(t *testing.T) {
	const half = int64(1) << 32
	_, err := Unwrap33([]int64{0, half}, 7)
	if err == nil {
		t.Fatal("expected ambiguity fault")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Stage != media.StageUnwrap || f.Track != 7 {
		t.Fatalf("want unwrap fault on track 7, got %v", err)
	}
}

func TestUnwrap33OutOfRange(t *testing.T) {
	if _, err := Unwrap33([]int64{-1}, 1); err == nil {
		t.Fatal("expected range fault")
	}
	if _, err := Unwrap33([]int64{1 << 33}, 1); err == nil {
		t.Fatal("expected range fault")
	}
}
