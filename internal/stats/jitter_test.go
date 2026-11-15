package stats

import (
	"math"
	"testing"
	"time"
)

func almostEqual(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %.12f want %.12f", got, want)
	}
}

func TestTrackerRFC3550JitterAndLoss(t *testing.T) {
	tracker := NewTracker(90000)
	start := time.Unix(100, 0)
	tracker.Update(0, 0, start)
	tracker.Update(1, 900, start.Add(10*time.Millisecond))
	tracker.Update(3, 2700, start.Add(25*time.Millisecond))

	got := tracker.Summary()
	if got.Received != 3 || got.Lost != 1 {
		t.Fatalf("received/lost = %d/%d", got.Received, got.Lost)
	}
	if got.Jitter.Samples != 2 {
		t.Fatalf("samples = %d", got.Jitter.Samples)
	}
	almostEqual(t, got.Jitter.P50Ms, 0)
	almostEqual(t, got.Jitter.P95Ms, 5)
	almostEqual(t, got.Jitter.MaxMs, 5)
	almostEqual(t, got.Jitter.MeanMs, 2.5)
	almostEqual(t, got.Jitter.RFC3550Ms, 5.0/16.0)
}

func TestTrackerOutOfOrderDuplicateAndWrap(t *testing.T) {
	tracker := NewTracker(48000)
	start := time.Unix(200, 0)
	packets := []struct {
		seq  uint16
		ts   uint32
		wait time.Duration
	}{
		{65534, 0, 0},
		{65535, 960, time.Millisecond},
		{0, 1920, time.Millisecond},
		{1, 2880, time.Millisecond},
		{0, 1920, time.Millisecond},
	}
	at := start
	for _, packet := range packets {
		at = at.Add(packet.wait)
		tracker.Update(packet.seq, packet.ts, at)
	}
	summary := tracker.Summary()
	if summary.Received != 4 || summary.Duplicated != 1 || summary.Lost != 0 {
		t.Fatalf("wrap summary: %+v", summary)
	}

	outOfOrder := NewTracker(48000)
	for i, seq := range []uint16{10, 11, 13, 12} {
		outOfOrder.Update(seq, uint32(i*960), start.Add(time.Duration(i)*time.Millisecond))
	}
	summary = outOfOrder.Summary()
	if summary.OutOfOrder != 1 || summary.Lost != 0 {
		t.Fatalf("out-of-order summary: %+v", summary)
	}
}

func TestTrackerLossUsesExtendedHighestSequence(t *testing.T) {
	tracker := NewTracker(48000)
	start := time.Unix(300, 0)
	for i, seq := range []uint16{65534, 65535, 2} {
		tracker.Update(seq, uint32(i*960), start.Add(time.Duration(i)*time.Millisecond))
	}
	summary := tracker.Summary()
	if summary.Lost != 2 {
		t.Fatalf("lost = %d, want 2", summary.Lost)
	}
}
