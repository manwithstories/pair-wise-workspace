package clock

import (
	"math"
	"testing"

	"medpipe/internal/model"
)

func TestUnwrapSingleWrap(t *testing.T) {
	raw := []uint64{65500, 65516, 65532, 12, 28, 44}
	r, err := Unwrap(raw, UnwrapOptions{Bits: 16, NominalDelta: 16})
	if err != nil {
		t.Fatal(err)
	}
	if r.Wraps != 1 {
		t.Fatalf("wraps = %d, want 1", r.Wraps)
	}
	want := []uint64{65500, 65516, 65532, 65536 + 12, 65536 + 28, 65536 + 44}
	for i := range want {
		if r.Unwrapped[i] != want[i] {
			t.Fatalf("unwrapped[%d] = %d, want %d", i, r.Unwrapped[i], want[i])
		}
	}
	for i, d := range r.Deltas {
		if d != 16 {
			t.Fatalf("delta[%d] = %d, want 16", i, d)
		}
	}
}

func TestUnwrapMultipleWrapsAndDeterminism(t *testing.T) {
	// 8-bit counter wrapping twice with drift: steps of 100 and 110.
	raw := []uint64{200, 44, 150, 255, 106, 212}
	a, err := Unwrap(raw, UnwrapOptions{Bits: 8, NominalDelta: 100})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Unwrap(raw, UnwrapOptions{Bits: 8, NominalDelta: 100})
	if err != nil {
		t.Fatal(err)
	}
	if a.Wraps != b.Wraps || a.DriftPPM != b.DriftPPM {
		t.Fatalf("repeated unwrap differs: %+v vs %+v", a, b)
	}
	for i := range a.Unwrapped {
		if a.Unwrapped[i] != b.Unwrapped[i] {
			t.Fatalf("unwrapped differs at %d", i)
		}
	}
	if math.Abs(a.DriftPPM) < 1e3 {
		t.Errorf("drift = %v ppm, expected a clear positive drift", a.DriftPPM)
	}
}

func TestUnwrapAmbiguousHalfRange(t *testing.T) {
	_, err := Unwrap([]uint64{100, 33000}, UnwrapOptions{Bits: 16, NominalDelta: 100})
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !model.IsKind(err, model.KindAmbiguous) {
		t.Fatalf("kind = %v, want ambiguous: %v", err, err)
	}
}

func TestUnwrapOutOfRangeAndNonAdvancing(t *testing.T) {
	if _, err := Unwrap([]uint64{65535, 70000}, UnwrapOptions{Bits: 16}); !model.IsKind(err, model.KindCorrupt) {
		t.Fatalf("out-of-range: got %v", err)
	}
	if _, err := Unwrap([]uint64{5, 5}, UnwrapOptions{Bits: 16}); !model.IsKind(err, model.KindAmbiguous) {
		t.Fatalf("non-advancing: got %v", err)
	}
	if _, err := Unwrap([]uint64{1, 2}, UnwrapOptions{Bits: 12}); !model.IsKind(err, model.KindInput) {
		t.Fatalf("bad width: got %v", err)
	}
}

func TestUnwrap64RejectsRegression(t *testing.T) {
	if _, err := Unwrap([]uint64{10, 9}, UnwrapOptions{Bits: 64}); !model.IsKind(err, model.KindAmbiguous) {
		t.Fatalf("regression: got %v", err)
	}
}

func TestEstimateDriftPPM(t *testing.T) {
	// Ticks advance at 1000 per frame with no drift.
	var flat []uint64
	for i := 0; i < 100; i++ {
		flat = append(flat, uint64(i*1000))
	}
	if d := EstimateDriftPPM(flat, 1000); math.Abs(d) > 1e-6 {
		t.Errorf("flat drift = %v", d)
	}
	// 200 ppm slow: 1000.2 ticks per frame.
	var slow []uint64
	for i := 0; i < 500; i++ {
		slow = append(slow, uint64(math.Round(float64(i)*1000.2)))
	}
	if d := EstimateDriftPPM(slow, 1000); math.Abs(d-200) > 25 {
		t.Errorf("slow drift = %v ppm, want ~200", d)
	}
	// At a finer tick resolution the same 200 ppm is resolved exactly.
	var fine []uint64
	for i := 0; i < 2000; i++ {
		fine = append(fine, uint64(math.Round(float64(i)*100000.2)))
	}
	if d := EstimateDriftPPM(fine, 100000); math.Abs(d-2) > 1 {
		t.Errorf("fine drift = %v ppm, want ~2", d)
	}
}
