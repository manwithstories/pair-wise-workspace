package rate

import (
	"testing"

	"mediascan/internal/clock"
)

func testTrace(t *testing.T, clk clock.Clock) *Trace {
	t.Helper()
	tr, err := NewTrace(clk, []Sample{
		{Tick: 0, Bandwidth: 1000},
		{Tick: clk.TicksPerFrame() * 4, Bandwidth: 400},  // congestion
		{Tick: clk.TicksPerFrame() * 8, Bandwidth: 1200}, // recovery
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestTracePiecewise(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	tr := testTrace(t, clk)
	if got := tr.BandwidthAt(clk.TicksPerFrame() * 2); got != 1000 {
		t.Fatalf("bw = %d, want 1000", got)
	}
	if got := tr.BandwidthAt(clk.TicksPerFrame() * 5); got != 400 {
		t.Fatalf("bw = %d, want 400", got)
	}
	if got := tr.BandwidthAt(clk.TicksPerFrame() * 9); got != 1200 {
		t.Fatalf("bw = %d, want 1200", got)
	}
}

func TestTraceRejects(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	if _, err := NewTrace(clk, nil); err == nil {
		t.Fatal("empty trace rejected")
	}
	if _, err := NewTrace(clk, []Sample{{Tick: 1, Bandwidth: 100}, {Tick: 1, Bandwidth: 200}}); err == nil {
		t.Fatal("non-increasing ticks rejected")
	}
	if _, err := NewTrace(clk, []Sample{{Tick: 0, Bandwidth: 0}}); err == nil {
		t.Fatal("zero bandwidth rejected")
	}
}

func TestControllerBackoffAndRecovery(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	tr := testTrace(t, clk)
	cfg := DefaultConfig(900)
	c, err := NewController(cfg, tr, clk, 900)
	if err != nil {
		t.Fatal(err)
	}
	var targets []int
	for i := 0; i < 24; i++ {
		// healthy frame size ~ target 900kbps -> ~3750 bytes per frame
		d, err := c.Step(i, 3750)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, d.TargetKbps)
	}
	// during the congested band (slots 4..7) target must back off
	if targets[5] >= 900 {
		t.Fatalf("controller did not back off: %v", targets)
	}
	// after bandwidth returns at slot 8 the additive probe must climb
	// above the congestion trough within the remaining window
	trough := targets[5]
	for _, v := range targets[5:8] {
		if v < trough {
			trough = v
		}
	}
	if targets[23] <= trough {
		t.Fatalf("controller did not recover above trough %d: %v", trough, targets)
	}
}

func TestLossDropsActualBitrate(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	tr, _ := NewTrace(clk, []Sample{{Tick: 0, Bandwidth: 1000}})
	c, _ := NewController(DefaultConfig(900), tr, clk, 900)
	ok, err := c.Step(0, 3750)
	if err != nil {
		t.Fatal(err)
	}
	lost, err := c.Step(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok.ActualKbps <= 0 {
		t.Fatalf("delivered frame should have actual bitrate, got %d", ok.ActualKbps)
	}
	if lost.ActualKbps != 0 {
		t.Fatalf("lost frame actual = %d, want 0", lost.ActualKbps)
	}
	if lost.ErrorPctX10 == 0 {
		t.Fatal("lost frame must show nonzero target error")
	}
}

func TestSummaryAndDeterminism(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	run := func() Summary {
		tr := testTrace(t, clk)
		c, _ := NewController(DefaultConfig(900), tr, clk, 900)
		var ds []Decision
		for i := 0; i < 12; i++ {
			d, _ := c.Step(i, 3750)
			ds = append(ds, d)
		}
		return Summarise(900, ds)
	}
	a, b := run(), run()
	if a.MeanTarget != b.MeanTarget || a.MeanActual != b.MeanActual ||
		a.MeanErrorPctX10 != b.MeanErrorPctX10 || a.TargetKbps != 900 {
		t.Fatalf("non-deterministic summaries: %+v vs %+v", a, b)
	}
	if a.MeanTarget <= 0 || a.MeanActual <= 0 {
		t.Fatalf("summary means invalid: %+v", a)
	}
}

func TestTargetBoundsEnforced(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	tr, _ := NewTrace(clk, []Sample{{Tick: 0, Bandwidth: 50}})
	cfg := DefaultConfig(900)
	c, _ := NewController(cfg, tr, clk, 900)
	for i := 0; i < 20; i++ {
		d, _ := c.Step(i, 100)
		if d.TargetKbps < cfg.FloorKbps || d.TargetKbps > cfg.CeilKbps {
			t.Fatalf("target %d outside bounds", d.TargetKbps)
		}
	}
}
