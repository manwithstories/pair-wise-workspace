package media

import "testing"

func TestWrapUnwrapperCanonicalWrap(t *testing.T) {
	u := NewWrapUnwrapper(0)
	// Start close to the 32-bit boundary so the third packet crosses it.
	seq := uint16(10)
	if r, ok := u.Push(seq, 0xFFFFF000); !ok || r.Unwrapped != 0xFFFFF000 {
		t.Fatalf("first push = %d, ok=%v", r.Unwrapped, ok)
	}
	seq++
	if r, ok := u.Push(seq, 0xFFFFFF80); !ok || r.Wrapped {
		t.Fatal("second push unexpected wrap")
	}
	seq++
	r, ok := u.Push(seq, 0x00000100)
	if !ok || !r.Wrapped {
		t.Fatalf("wrap not detected, ok=%v", ok)
	}
	if want := Ticks(WrapMod + 0x100); r.Unwrapped != want {
		t.Fatalf("unwrapped=%d want %d", r.Unwrapped, want)
	}
	if len(u.Events()) != 1 {
		t.Fatalf("events=%d want 1", len(u.Events()))
	}
}

func TestWrapUnwrapperRejectsAmbiguousBackward(t *testing.T) {
	u := NewWrapUnwrapper(1000)
	if _, ok := u.Push(1, 5000); !ok {
		t.Fatal("first push rejected")
	}
	// Same sequence, raw value slightly backwards: not a full rotation.
	if _, ok := u.Push(1, 4900); ok {
		t.Fatal("ambiguous backward step was accepted")
	}
}

func TestWrapUnwrapperNonZeroBase(t *testing.T) {
	base := FromSeconds(5)
	u := NewWrapUnwrapper(base)
	r, ok := u.Push(0, 7)
	if !ok || r.Unwrapped != base+7 {
		t.Fatalf("base mapping got %d want %d", r.Unwrapped, base+7)
	}
}

func TestDriftCorrectorConverges(t *testing.T) {
	// Steady 30-tick clock offset: the EWMA estimator must converge to it
	// while every individual step stays inside the bounded maxStep.
	d := NewDriftCorrector(0.25, 2)
	var corr Ticks
	for i := 0; i < 80; i++ {
		obs := Ticks(1000 + i*100 + 30)
		ref := Ticks(1000 + i*100)
		var c Correction
		corr, c = d.Update(uint16(i), DriftSample{Observed: obs, Reference: ref})
		if c.Amount > 2 || c.Amount < -2 {
			t.Fatalf("step %d exceeded bound: %d", i, c.Amount)
		}
	}
	if d.TotalCorrection() <= 0 {
		t.Fatal("no correction accumulated")
	}
	res := corr - Ticks(1000+79*100)
	if res > 5 || res < -5 {
		t.Fatalf("residual %d did not converge toward zero", res)
	}
}

func TestDriftCorrectorBoundedStep(t *testing.T) {
	// A single 1 000-tick outlier may not move the timeline by more than
	// maxStep on one sample.
	d := NewDriftCorrector(0.5, 4)
	_, c := d.Update(0, DriftSample{Observed: 1000, Reference: 0})
	if c.Amount > 4 || c.Amount < -4 {
		t.Fatalf("outlier step unbounded: %d", c.Amount)
	}
}

func TestTickConversions(t *testing.T) {
	if got := FromSeconds(2); got != 180000 {
		t.Fatalf("2s = %d ticks", got)
	}
	if got := (Ticks(90000)).Milliseconds(); got != 1000 {
		t.Fatalf("90000 ticks = %d ms", got)
	}
}
