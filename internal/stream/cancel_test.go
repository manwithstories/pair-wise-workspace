package stream

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSlowConsumerBlocksProducerUntilSpace pins the backpressure contract from
// the brief: with a full queue and an idle consumer, the producer must stay
// blocked rather than allocate, and a later receive must release it.
func TestSlowConsumerBlocksProducerUntilSpace(t *testing.T) {
	q, err := New[int](2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := q.Send(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	var (
		mu        sync.Mutex
		unblocked bool
	)
	done := make(chan struct{})
	go func() {
		if err := q.Send(ctx, 99); err == nil {
			mu.Lock()
			unblocked = true
			mu.Unlock()
		}
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	blocked := !unblocked
	mu.Unlock()
	if !blocked {
		t.Fatal("producer was not blocked by a slow consumer")
	}
	if peak := q.Stats().Peak; peak != 2 {
		t.Fatalf("peak=%d while queue should be full", peak)
	}
	if _, ok := q.Recv(); !ok {
		t.Fatal("receive failed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer did not resume after space freed")
	}
}

// TestCancelClosesAllStages checks that cancelling through the queue's context
// unblocks every waiter and that subsequent sends fail with the queue stage.
func TestCancelClosesAllStages(t *testing.T) {
	q, err := New[int](1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Send(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := q.RecvCtx(ctx); err != nil {
		t.Fatalf("cancelled receive should have returned the buffered item first: %v", err)
	}
	if _, err := q.RecvCtx(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	select {
	case <-q.Done():
	default:
		t.Fatal("queue not closed after cancellation")
	}
	if err := q.Send(context.Background(), 2); err == nil {
		t.Fatal("send after cancellation succeeded")
	}
}
