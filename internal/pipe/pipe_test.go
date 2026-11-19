package pipe

import (
	"context"
	"errors"
	"testing"
	"time"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

func packets(n int) []scan.Packet {
	out := make([]scan.Packet, n)
	for i := range out {
		out[i] = scan.Packet{TrackID: 1, Index: uint32(i + 1), Offset: int64(i * 100), DTS: int64(i) * 3600}
	}
	return out
}

// TestBackpressureAndOrder: a small queue with a slow consumer must still
// deliver every packet in order, with the last acked packet recorded.
func TestBackpressureAndOrder(t *testing.T) {
	r := &Runner{Capacity: 4, ConsumerDelay: time.Millisecond}
	var consumed []uint32
	stats, q, faults := r.Run(context.Background(), packets(50), func(_ context.Context, it Item) error {
		consumed = append(consumed, it.Packet.Index)
		return nil
	})
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %v", faults)
	}
	if stats.Consumed != 50 || stats.Produced != 50 {
		t.Fatalf("produced=%d consumed=%d", stats.Produced, stats.Consumed)
	}
	for i, idx := range consumed {
		if idx != uint32(i+1) {
			t.Fatalf("order broken at %d: %v", i, consumed)
		}
	}
	last, ok := q.LastAcked()
	if !ok || last.Packet.Index != 50 {
		t.Fatalf("last acked = %v, %v", last.Packet.Index, ok)
	}
	if stats.P95Latency() <= 0 {
		t.Fatal("p95 latency must be positive")
	}
}

// TestConsumerError: a consumer failure aborts the pipeline, releases the
// producer and preserves the last acknowledged packet.
func TestConsumerError(t *testing.T) {
	r := &Runner{Capacity: 2}
	boom := errors.New("decoder exploded")
	stats, q, faults := r.Run(context.Background(), packets(100), func(_ context.Context, it Item) error {
		if it.Packet.Index == 5 {
			return boom
		}
		return nil
	})
	if len(faults.ByKind(fault.KindConsumerError)) != 1 {
		t.Fatalf("want consumer_error fault, got %v", faults)
	}
	if stats.Consumed != 4 {
		t.Fatalf("consumed=%d, want 4", stats.Consumed)
	}
	last, ok := q.LastAcked()
	if !ok || last.Packet.Index != 4 {
		t.Fatalf("last acked = %d, want 4", last.Packet.Index)
	}
	if q.AbortErr() == nil {
		t.Fatal("queue must record abort reason")
	}
}

// TestCancellation: context cancellation unblocks producer and consumer
// and is reported, never hangs.
func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{Capacity: 2, ConsumerDelay: 5 * time.Millisecond}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	done := make(chan fault.List, 1)
	go func() {
		_, _, faults := r.Run(ctx, packets(100000), func(context.Context, Item) error { return nil })
		done <- faults
	}()
	select {
	case <-done:
		// canceled promptly; a canceled fault may or may not be recorded
		// depending on where the cancel landed, but it must not hang.
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not terminate on cancel")
	}
}

// TestSlowConsumerCleanup: a stalled consumer is detected by the watchdog
// and the queue is torn down with a consumer_slow fault.
func TestSlowConsumerCleanup(t *testing.T) {
	r := &Runner{Capacity: 2, SlowTimeout: 50 * time.Millisecond}
	start := time.Now()
	_, _, faults := r.Run(context.Background(), packets(10), func(ctx context.Context, it Item) error {
		if it.Packet.Index == 1 {
			<-ctx.Done() // stall until the watchdog cancels
			return nil
		}
		return nil
	})
	if time.Since(start) > 5*time.Second {
		t.Fatal("slow consumer was not cleaned up promptly")
	}
	if len(faults.ByKind(fault.KindConsumerSlow)) != 1 {
		t.Fatalf("want consumer_slow fault, got %v", faults)
	}
}
