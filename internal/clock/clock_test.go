package clock

import (
	"testing"

	"tscheck/internal/model"
)

const max33 = int64(1) << 33

func TestPTSBeforePCRIsUnrecoverable(t *testing.T) {
	c := NewClock()
	ts := c.UnwrapPTS(12345)
	if ts.Anchored || ts.Epoch != -1 {
		t.Fatalf("pts before pcr must be unanchored: %+v", ts)
	}
}

func TestPCRWrapExtendsEpoch(t *testing.T) {
	c := NewClock()
	first := c.ObservePCR(max33-45000, 0) // 0.5s before wrap
	second := c.ObservePCR(45000, 0)      // 0.5s after wrap
	if c.Wraps() != 1 || second.Epoch != 1 {
		t.Fatalf("wraps=%d epoch=%d", c.Wraps(), second.Epoch)
	}
	want := model.PTS90kToTicks(max33 + 45000)
	if second.Ticks != want {
		t.Fatalf("second pcr = %d, want %d", second.Ticks, want)
	}
	if second.Ticks <= first.Ticks {
		t.Fatal("unwrapped pcr must be monotonic across wrap")
	}
}

func TestPTSAnchoredAcrossWrap(t *testing.T) {
	c := NewClock()
	c.ObservePCR(max33-90000, 0) // 1s before wrap
	c.ObservePCR(90000, 0)       // 1s after wrap
	ts := c.UnwrapPTS(45000)     // raw pts just after wrap
	if !ts.Anchored || ts.Epoch != 1 {
		t.Fatalf("pts = %+v", ts)
	}
	if want := model.PTS90kToTicks(max33 + 45000); ts.Ticks != want {
		t.Fatalf("pts ticks = %d, want %d", ts.Ticks, want)
	}
	// A PTS from before the wrap observed late still resolves nearby.
	pre := c.UnwrapPTS(max33 - 45000)
	if pre.Ticks != model.PTS90kToTicks(max33-45000) {
		t.Fatalf("pre-wrap pts = %d", pre.Ticks)
	}
}

func TestDriftClamped(t *testing.T) {
	d := NewDriftEstimator(100) // ±100 ppm
	d.Observe(0, 0)
	// Observed clock runs 1% fast: must clamp to +100 ppm.
	got := d.Observe(model.Ticks(27_000_000), model.Ticks(27_270_000))
	if got != 1.0001 {
		t.Fatalf("drift = %v, want 1.0001", got)
	}
}

func TestDriftNominal(t *testing.T) {
	d := NewDriftEstimator(500)
	d.Observe(0, 0)
	got := d.Observe(model.Ticks(27_000_000), model.Ticks(27_000_000+2700))
	if got < 0.9999 || got > 1.0001 {
		t.Fatalf("drift = %v", got)
	}
	// Non-monotonic reference keeps the last estimate.
	if got2 := d.Observe(model.Ticks(-100), model.Ticks(100)); got2 != got {
		t.Fatalf("drift changed on bad reference: %v", got2)
	}
}
