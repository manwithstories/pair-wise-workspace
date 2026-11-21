package buffer

import "testing"

// feed simulates receiving at rate bps from t0 to t1 in 100 ms steps,
// returning the last buffer target.
func feed(b *Buffer, t0, t1 int64, bps float64) int64 {
	for t := t0; t < t1; t += 100 {
		b.Update(t, int64(bps/8/10)) // bytes per 100 ms
	}
	return t1
}

func TestFallbackWhenStale(t *testing.T) {
	b := NewBuffer(NewEstimator(1000), 20, 500, 120, 2_000_000)
	if got := b.Update(0, 100); got != 120 {
		t.Fatalf("thin estimate must fall back: got %d", got)
	}
	feed(b, 100, 1000, 2_000_000)
	// jump far ahead so the window empties: estimate stale again
	if got := b.Update(5000, 100); got != 120 {
		t.Fatalf("stale estimate must fall back: got %d", got)
	}
}

func TestAdaptsWithinBounds(t *testing.T) {
	b := NewBuffer(NewEstimator(1000), 20, 500, 120, 2_000_000)
	feed(b, 0, 2000, 2_000_000)
	hi := b.Target(0, false)
	_ = hi
	feed(b, 2000, 4000, 500_000) // quarter of ref rate
	got := b.Update(4000, 1)
	if got <= 120 || got > 500 {
		t.Fatalf("low bandwidth should deepen buffer within bounds: %d", got)
	}
}

func TestAudioPriorityOnSuddenDrop(t *testing.T) {
	b := NewBuffer(NewEstimator(1000), 20, 500, 120, 2_000_000)
	feed(b, 0, 2000, 2_000_000)
	feed(b, 2000, 2100, 2_000_000)
	feed(b, 2100, 3200, 600_000) // sudden drop
	if !b.AudioPriority {
		t.Fatal("sudden drop must raise audio priority")
	}
	feed(b, 3200, 5000, 3_000_000) // recovery
	if b.AudioPriority {
		t.Fatal("recovered bandwidth must clear audio priority")
	}
}

func TestBoundsRespected(t *testing.T) {
	b := NewBuffer(NewEstimator(1000), 20, 500, 120, 2_000_000)
	if got := b.Target(100_000_000, true); got != 20 {
		t.Fatalf("huge rate must clamp to min: %d", got)
	}
	if got := b.Target(1, true); got != 500 {
		t.Fatalf("tiny rate must clamp to max: %d", got)
	}
}
