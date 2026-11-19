package timeline

import "testing"

func TestMonotonicMapping(t *testing.T) {
	c := NewClock(90000, 5000)
	if ns, _, ev := c.Map(1000); ns != 0 || ev != nil {
		t.Fatalf("first map: ns=%d ev=%v", ns, ev)
	}
	if !c.Estimated {
		t.Fatal("should be estimated before any Sender Report")
	}
	ns, _, ev := c.Map(1000 + 90000) // +1s
	if ev != nil || ns != 1_000_000_000 {
		t.Fatalf("ns=%d ev=%v", ns, ev)
	}
}

func TestWraparound(t *testing.T) {
	c := NewClock(48000, 5000)
	c.Map(0xFFFFFFF0)
	ns, ext, ev := c.Map(0x00000010) // wrapped, +32 ticks
	if ev == nil || ev.Kind != EvWrap {
		t.Fatalf("want wrap event, got %+v", ev)
	}
	if ext != 1<<32+0x10 {
		t.Fatalf("ext=%d", ext)
	}
	want := int64(32) * 1_000_000_000 / 48000
	if ns != want {
		t.Fatalf("ns=%d want %d", ns, want)
	}
	// Continues monotonically after wrap.
	ns2, _, ev := c.Map(0x20)
	if ev != nil || ns2 <= ns {
		t.Fatalf("after wrap: ns=%d ev=%v", ns2, ev)
	}
}

func TestBackwardCorrection(t *testing.T) {
	c := NewClock(90000, 5000)
	c.Map(0)
	before, _, _ := c.Map(90000) // t=1s
	_, _, ev := c.Map(45000)     // jumps back 0.5s
	if ev == nil || ev.Kind != EvBackward {
		t.Fatalf("want backward event, got %+v", ev)
	}
	if ev.DeviationNs != 500_000_000 {
		t.Fatalf("deviation=%d", ev.DeviationNs)
	}
	// Mapping stays monotonic: next packet maps after `before`.
	after, _, _ := c.Map(45000 + 90000)
	if after <= before {
		t.Fatalf("not monotonic: before=%d after=%d", before, after)
	}
}

func TestSenderReportAnchorAndDrift(t *testing.T) {
	c := NewClock(90000, 1000) // 1000 ppm tolerance
	c.Map(0)
	if ev := c.AddSenderReport(0, 5_000_000_000, 0); ev != nil {
		t.Fatalf("first SR: %+v", ev)
	}
	if c.Estimated {
		t.Fatal("should be anchored after Sender Report")
	}
	ns, _, _ := c.Map(90000)
	if ns != 6_000_000_000 {
		t.Fatalf("anchored ns=%d", ns)
	}
	// Second SR consistent with nominal rate: no drift.
	if ev := c.AddSenderReport(90000, 6_000_000_000, 90000); ev != nil {
		t.Fatalf("consistent SR: %+v", ev)
	}
	// Third SR implies ~10% drift: flagged.
	ev := c.AddSenderReport(180000, 7_100_000_000, 180000)
	if ev == nil || ev.Kind != EvDrift {
		t.Fatalf("want drift event, got %+v", ev)
	}
}
