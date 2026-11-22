package stream

import (
	"context"
	"testing"
	"time"

	"mediapack/internal/clock"
)

func makeItem(track uint64, payload int) Item {
	return Item{Track: track, ByteOff: int64(track) * 10, Payload: make([]byte, payload)}
}

func TestEnforceByteBudget(t *testing.T) {
	// Limit holds exactly three 100-byte items incl. overhead.
	const limit = 3 * (100 + PerItemOverhead)
	q := NewBoundedQueue(limit)
	for i := 0; i < 3; i++ {
		ok, err := q.TryEnqueue(makeItem(1, 100))
		if err != nil || !ok {
			t.Fatalf("item %d should fit: ok=%v err=%v", i, ok, err)
		}
	}
	ok, err := q.TryEnqueue(makeItem(1, 100))
	if err != nil || ok {
		t.Fatalf("fourth item must be rejected without queuing: ok=%v err=%v", ok, err)
	}
	s := q.Stats()
	if s.Bytes > limit {
		t.Fatalf("charged bytes %d exceed limit %d", s.Bytes, limit)
	}
	if s.PeakBytes > limit {
		t.Fatalf("peak %d exceeded limit %d", s.PeakBytes, limit)
	}
	if s.Dropped != 0 || s.Enqueued != 3 {
		t.Fatalf("accounting wrong: %+v", s)
	}
}

func TestOversizedItemRejected(t *testing.T) {
	q := NewBoundedQueue(128)
	if _, err := q.TryEnqueue(makeItem(1, 500)); err == nil {
		t.Fatal("oversized item must error")
	} else if me, ok := clock.AsMediaError(err); !ok || me.Category != clock.CatOversized {
		t.Fatalf("want CatOversized, got %v", err)
	}
}

func TestFIFOOrderingNoDrops(t *testing.T) {
	q := NewBoundedQueue(1 << 20)
	ctx := context.Background()
	const n = 100
	for i := 0; i < n; i++ {
		it := makeItem(uint64(i%3+1), 16)
		it.ByteOff = int64(1000 + i) // unique ordering marker
		if err := q.Enqueue(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		it, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if it.ByteOff != int64(1000+i) {
			t.Fatalf("FIFO order broken at %d: off=%d", i, it.ByteOff)
		}
	}
	if q.Stats().Enqueued != n {
		t.Fatal("items lost through the queue")
	}
}

func TestBackpressureBlocksProducer(t *testing.T) {
	// Room for two items; a third Enqueue must block until the consumer
	// drains, proving back-pressure reaches the producer.
	q := NewBoundedQueue(2 * (32 + PerItemOverhead))
	ctx := context.Background()
	if err := q.Enqueue(ctx, makeItem(1, 32)); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, makeItem(1, 32)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Enqueue(ctx, makeItem(1, 32)) }()
	select {
	case <-done:
		t.Fatal("producer must block while queue is full")
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("producer should complete after drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer never unblocked after consumer drained")
	}
}

func TestSlowConsumerCancellation(t *testing.T) {
	// Fill the queue, then cancel the producer context while it blocks.
	q := NewBoundedQueue(1 * (32 + PerItemOverhead))
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Enqueue(ctx, makeItem(1, 32)); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- q.Enqueue(ctx, makeItem(1, 32)) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-blocked:
		me, ok := clock.AsMediaError(err)
		if !ok || me.Category != clock.CatCancelled {
			t.Fatalf("want CatCancelled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled producer never returned")
	}
	// The already-queued item must survive: no silent loss.
	it, err := q.Dequeue(context.Background())
	if err != nil || it.Track != 1 {
		t.Fatalf("committed item must survive cancellation: %v %v", it, err)
	}
}

func TestCloseDrainsThenErrors(t *testing.T) {
	q := NewBoundedQueue(1 << 20)
	ctx := context.Background()
	if err := q.Enqueue(ctx, makeItem(1, 8)); err != nil {
		t.Fatal(err)
	}
	q.Close()
	// Queued item still delivered.
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("drained item must be delivered: %v", err)
	}
	// Subsequent dequeue reports closed, subsequent enqueue fails.
	if _, err := q.Dequeue(ctx); err == nil {
		t.Fatal("dequeue on closed drained queue must error")
	}
	if err := q.Enqueue(ctx, makeItem(1, 8)); err == nil {
		t.Fatal("enqueue on closed queue must error")
	}
}

func TestPeakAccountingNeverExceedsLimit(t *testing.T) {
	const limit = int64(4 * (64 + PerItemOverhead))
	q := NewBoundedQueue(limit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := 0; i < 200; i++ {
			if err := q.Enqueue(ctx, makeItem(uint64(i%2+1), 64)); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := q.Dequeue(ctx); err != nil {
			break
		}
		if q.Stats().PeakBytes > limit {
			t.Fatalf("peak %d crossed limit %d", q.Stats().PeakBytes, limit)
		}
	}
	<-producerDone
	if q.Stats().PeakBytes > limit {
		t.Fatalf("final peak %d crossed limit %d", q.Stats().PeakBytes, limit)
	}
}
