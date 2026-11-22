package pipeline

import (
	"testing"
	"time"

	"mediarun/internal/ingest"
)

func TestBoundedQueueBackPressure(t *testing.T) {
	q := NewBoundedQueue(2)
	if q.Cap() != 2 {
		t.Fatalf("cap = %d, want 2", q.Cap())
	}
	// Fill to capacity.
	for i := 0; i < 2; i++ {
		if err := q.Push(&Block{Samples: []*ingest.SampleEvent{{Index: int64(i)}}}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if q.Len() != 2 {
		t.Fatalf("len = %d, want 2", q.Len())
	}
	// A third push must block until a consumer drains one slot.
	done := make(chan error, 1)
	go func() { done <- q.Push(&Block{Samples: []*ingest.SampleEvent{{Index: 2}}}) }()
	select {
	case <-done:
		t.Fatal("push on full queue must block (no back-pressure)")
	case <-time.After(20 * time.Millisecond):
	}
	if _, ok := q.Pop(); !ok {
		t.Fatal("pop failed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked push should succeed after drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("push never unblocked after drain")
	}
}

func TestQueueCloseUnblocksConsumers(t *testing.T) {
	q := NewBoundedQueue(1)
	q.Close()
	if _, ok := q.Pop(); ok {
		t.Fatal("pop on closed empty queue must return false")
	}
	if err := q.Push(&Block{}); err != ErrQueueClosed {
		t.Fatalf("push after close = %v, want ErrQueueClosed", err)
	}
	// Idempotent close must not panic.
	q.Close()
}

func TestChunkBlockCeilings(t *testing.T) {
	cb := newChunkBlock()
	// Small samples should fill by chunk count, never breach the count limit.
	for i := 0; i < blockMaxChunks-1; i++ {
		if cb.add(&ingest.SampleEvent{Size: 1}) {
			t.Fatalf("block filled early at %d", i)
		}
	}
	if !cb.add(&ingest.SampleEvent{Size: 1}) {
		t.Fatal("block must fill at chunk count ceiling")
	}
	cb.reset()
	// A 4 MiB sample must fill the block by the byte ceiling immediately.
	big := &ingest.SampleEvent{Size: ingest.MaxBlockBytes}
	if !cb.add(big) {
		t.Fatal("4 MiB sample must trip the byte ceiling on first add")
	}
}
