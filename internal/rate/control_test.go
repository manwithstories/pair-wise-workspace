package rate

import (
	"math"
	"testing"
)

func newTestController(t *testing.T) *Controller {
	t.Helper()
	c, err := New(Config{TargetBPS: 1200000, FPS: 30, GOP: 30, PixelCount: 320 * 180})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestQPBoundsBudgetAndVBV(t *testing.T) {
	c := newTestController(t)
	if got := c.QPForGOP(30, 0.05); got < 1 || got > 51 {
		t.Fatalf("QP out of range: %d", got)
	}
	budget := c.BudgetBits(30)
	if budget < 1.2e6*0.5 || budget > 1.2e6*1.5 {
		t.Fatalf("unexpected GOP budget %.0f", budget)
	}
	c.Observe(10, 100000000, 30, 0.05)
	s := c.Snapshot()
	if s.LevelBits < 0 || s.LevelBits > s.CapacityBits {
		t.Fatalf("VBV escaped bounds: %+v", s)
	}
	if c.DeviationPct() <= 0 {
		t.Fatalf("expected positive over-rate deviation")
	}
}

func TestAdjustAndRestore(t *testing.T) {
	c := newTestController(t)
	if got := c.AdjustQP(100, 200, 20); got <= 20 {
		t.Fatalf("undersized trial should raise QP, got %d", got)
	}
	if got := c.AdjustQP(200, 100, 20); got >= 20 {
		t.Fatalf("oversized trial should lower QP, got %d", got)
	}
	c.Observe(24, 900000, 30, 0.05)
	c.Observe(28, 700000, 30, 0.05)
	s := c.Snapshot()
	c.Restore(s)
	if math.Abs(c.Snapshot().CumActual-s.CumActual) > 1e-6 {
		t.Fatal("restore mismatch")
	}
	c.ResetScene(30)
	if c.SceneResets() != 1 {
		t.Fatal("scene reset not counted")
	}
}
