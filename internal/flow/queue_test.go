package flow

import (
	"context"
	"errors"
	"testing"
	"time"

	"avcheck/internal/clock"
)

func makeBlock(frames, ch int) *clock.Block {
	return &clock.Block{Channels: ch, Samples: make([]int32, frames*ch), StartTick: 0}
}

func TestEnqueueDequeueAndByteCeiling(t *testing.T) {
	q := NewQueue(200) // each block is 80; third enqueue must block
	ctx := context.Background()

	if err := q.Enqueue(ctx, makeBlock(10, 2), 0); err != nil { // 80 bytes
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, makeBlock(10, 2), 1); err != nil { // 80 bytes
		t.Fatal(err)
	}
	if got := q.Stats().Bytes; got != 160 {
		t.Fatalf("bytes=%d, want 160", got)
	}

	// Third block (160 bytes) does not fit in the remaining 160 -> blocks
	// until a consumer drains the head.
	done := make(chan error, 1)
	go func() { done <- q.Enqueue(ctx, makeBlock(10, 2), 2) }()
	select {
	case err := <-done:
		t.Fatalf("enqueue should have blocked, returned %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	b, err := q.Dequeue(ctx)
	if err != nil || b == nil {
		t.Fatalf("dequeue: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked enqueue: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue never unblocked after drain")
	}
	if q.Stats().PeakBytes > 200 {
		t.Fatalf("peak=%d exceeds ceiling 200", q.Stats().PeakBytes)
	}
}

func TestRejectOversizedBlock(t *testing.T) {
	// Payload over the 64 KiB single-block cap is a bad-block error.
	q := NewQueue(clock.MaxBlockBytes * 2)
	big := &clock.Block{Channels: 1, Samples: make([]int32, clock.MaxBlockBytes/4+1)}
	err := q.Enqueue(context.Background(), big, 0)
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindBadBlock {
		t.Fatalf("err=%v, want bad_block", err)
	}

	// Block larger than the queue ceiling is a queue boundary violation.
	q2 := NewQueue(8)
	err = q2.Enqueue(context.Background(), makeBlock(100, 1), 0)
	if !errors.As(err, &ce) || ce.Kind != clock.KindQueueFull {
		t.Fatalf("err=%v, want queue_full", err)
	}
}

func TestBackpressureEventRecorded(t *testing.T) {
	q := NewQueue(80)
	ctx := context.Background()
	if err := q.Enqueue(ctx, makeBlock(10, 2), 0); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(15 * time.Millisecond)
		q.Dequeue(ctx)
	}()
	if err := q.Enqueue(ctx, makeBlock(10, 2), 1); err != nil {
		t.Fatal(err)
	}
	s := q.Stats()
	if s.BackpressureEv == 0 {
		t.Fatal("expected at least one backpressure event")
	}
}

func TestAdaptiveWindowStaysInBounds(t *testing.T) {
	q := NewQueue(1 << 20)
	ctx := context.Background()
	// No traffic yet -> minimum window.
	if w := q.WindowMS(); w != MinWindowMS {
		t.Fatalf("idle window=%d, want %d", w, MinWindowMS)
	}
	for i := 0; i < 200; i++ {
		q.Enqueue(ctx, makeBlock(10, 2), int64(i))
	}
	w := q.WindowMS()
	if w < MinWindowMS || w > MaxWindowMS {
		t.Fatalf("window=%d outside [%d,%d]", w, MinWindowMS, MaxWindowMS)
	}
}

func TestDrainAfterFullLosesNoAcceptedBlock(t *testing.T) {
	const blocks = 50
	q := NewQueue(blocks * 80)
	ctx := context.Background()
	for i := 0; i < blocks; i++ {
		if err := q.Enqueue(ctx, makeBlock(10, 2), int64(i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	got := 0
	for {
		b, err := q.Dequeue(ctx)
		if err != nil || b == nil {
			break
		}
		got++
		if got == blocks {
			break
		}
	}
	if got != blocks {
		t.Fatalf("drained %d, want %d (lost accepted blocks)", got, blocks)
	}
	if q.Stats().PeakBytes > blocks*80 {
		t.Fatal("peak bytes exceeded configured ceiling")
	}
}

func TestCloseUnblocksWaiters(t *testing.T) {
	q := NewQueue(80)
	ctx := context.Background()
	q.Enqueue(ctx, makeBlock(10, 2), 0)
	done := make(chan error, 1)
	go func() { done <- q.Enqueue(ctx, makeBlock(10, 2), 1) }()
	time.Sleep(10 * time.Millisecond)
	q.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close did not unblock enqueue waiter")
	}
}
