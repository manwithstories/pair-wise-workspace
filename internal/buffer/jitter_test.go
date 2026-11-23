package buffer

import (
	"testing"
)

func cfgForTest() Config {
	return Config{
		MinTargetNS:       10_000_000,
		MaxTargetNS:       500_000_000,
		InitTargetNS:      100_000_000,
		MemoryBudgetBytes: 1 << 20,
		CommitHorizonNS:   50_000_000,
	}
}

func TestTargetWithinBounds(t *testing.T) {
	b := New("v", cfgForTest())
	// Wildly irregular arrivals drive the target to the ceiling, not beyond.
	ns := int64(0)
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			ns += 1_000_000
		} else {
			ns += 40_000_000
		}
		b.Push(Sample{NS: ns, Bytes: 1})
	}
	if b.TargetNS() < 10_000_000 || b.TargetNS() > 500_000_000 {
		t.Fatalf("target out of bounds: %d", b.TargetNS())
	}
}

func TestTargetAdaptsUpWithJitter(t *testing.T) {
	steady := New("v", cfgForTest())
	ns := int64(0)
	for i := 0; i < 100; i++ {
		ns += 10_000_000
		steady.Push(Sample{NS: ns})
	}
	jittery := New("v", cfgForTest())
	ns = 0
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			ns += 1_000_000
		} else {
			ns += 19_000_000
		}
		jittery.Push(Sample{NS: ns})
	}
	if jittery.TargetNS() <= steady.TargetNS() {
		t.Fatalf("jittery target %d should exceed steady %d",
			jittery.TargetNS(), steady.TargetNS())
	}
}

func TestMemoryBudgetShedsSamples(t *testing.T) {
	cfg := cfgForTest()
	cfg.MemoryBudgetBytes = 1000
	cfg.CommitHorizonNS = 1 << 40 // never commit during the push loop
	b := New("v", cfg)
	dropped := 0
	for i := 0; i < 100; i++ {
		ns := int64(i) * 10_000_000
		res := b.Push(Sample{NS: ns, Bytes: 100})
		dropped += res.Dropped
	}
	if dropped == 0 {
		t.Fatalf("budget of 1000 bytes never shed a 100-byte sample")
	}
	if b.HeldBytes() > cfg.MemoryBudgetBytes+100 {
		t.Fatalf("held bytes %d exceed budget", b.HeldBytes())
	}
}

func TestLatePacketCannotRewriteCommittedInterval(t *testing.T) {
	cfg := cfgForTest()
	cfg.CommitHorizonNS = 20_000_000
	b := New("v", cfg)
	ns := int64(0)
	for i := 0; i < 10; i++ {
		ns += 10_000_000
		b.Push(Sample{NS: ns})
	}
	// The first sample's interval is now frozen.
	if !b.IsFrozen(10_000_000) {
		t.Fatalf("expected first interval frozen, committed=%v", b.Committed())
	}
	before := len(b.Committed())
	res := b.Push(Sample{NS: 10_000_000, Bytes: 999})
	if !res.Late {
		t.Fatalf("late packet not reported: %+v", res)
	}
	if len(b.Committed()) != before {
		t.Fatalf("committed intervals changed: %d -> %d", before, len(b.Committed()))
	}
	var late int
	for _, iv := range b.Committed() {
		late += iv.Late
	}
	if late != 1 {
		t.Fatalf("late count not recorded: %+v", b.Committed())
	}
}

func TestFlushCommitsRemainder(t *testing.T) {
	cfg := cfgForTest()
	cfg.CommitHorizonNS = 1 << 40
	b := New("v", cfg)
	for i := 0; i < 5; i++ {
		b.Push(Sample{NS: int64(i) * 10_000_000})
	}
	if len(b.Committed()) != 0 {
		t.Fatalf("nothing should commit under a huge horizon")
	}
	out := b.Flush()
	if len(out) != 5 {
		t.Fatalf("flush produced %d intervals", len(out))
	}
	if b.HeldBytes() != 0 {
		t.Fatalf("held bytes after flush = %d", b.HeldBytes())
	}
}

func TestCommittedRangeError(t *testing.T) {
	b := New("v", cfgForTest())
	_, _, _, err := b.CommittedRange()
	if err == nil {
		t.Fatalf("empty buffer should report insufficient input")
	}
}
