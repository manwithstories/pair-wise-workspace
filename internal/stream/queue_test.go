package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"avpipe/internal/media"
)

func TestQueueBoundAndBackpressure(t *testing.T) {
	q, err := NewQueue[int](3)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := q.Push(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if q.Len() != 3 || q.Cap() != 3 {
		t.Fatalf("len=%d cap=%d", q.Len(), q.Cap())
	}

	// The next push must block while the queue is full.
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, 99) }()
	select {
	case err := <-done:
		t.Fatalf("push did not block: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if q.Stats().Backpressure == 0 {
		t.Fatal("backpressure not counted")
	}
	if v, ok := q.TryPop(); !ok || v != 0 {
		t.Fatalf("TryPop=%d,%v", v, ok)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("push stayed blocked after pop")
	}
	st := q.Stats()
	if st.HighWater > st.Capacity {
		t.Fatalf("high water %d > capacity %d", st.HighWater, st.Capacity)
	}
	if st.Len > st.Capacity {
		t.Fatalf("len %d > capacity", st.Len)
	}
}

func TestQueueCapacityValidation(t *testing.T) {
	if _, err := NewQueue[int](0); err == nil {
		t.Fatal("zero capacity accepted")
	}
	if _, err := NewQueue[int](-2); err == nil {
		t.Fatal("negative capacity accepted")
	}
	if _, err := NewQueue[int](4); err != nil {
		t.Fatal(err)
	}
}

func TestQueueFIFO(t *testing.T) {
	q, _ := NewQueue[int](4)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := q.Push(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	for want := 1; want <= 3; want++ {
		got, err := q.Pop(ctx)
		if err != nil || got != want {
			t.Fatalf("pop=%d err=%v want %d", got, err, want)
		}
	}
}

func TestQueueCancellationUnblocks(t *testing.T) {
	q, _ := NewQueue[int](1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Push(ctx, 1); err != nil {
		t.Fatal(err)
	}
	pushErr := make(chan error, 1)
	go func() { pushErr <- q.Push(ctx, 2) }()

	// Cancel while the producer is blocked on a full queue.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-pushErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("push err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled push stayed blocked")
	}
	// The item already in the queue is still readable: cancellation must not
	// discard the last complete block.
	v, ok := q.TryPop()
	if !ok || v != 1 {
		t.Fatalf("last block lost: v=%d ok=%v", v, ok)
	}
}

func TestQueueCancelPop(t *testing.T) {
	q, _ := NewQueue[int](2)
	ctx, cancel := context.WithCancel(context.Background())
	popErr := make(chan error, 1)
	go func() { _, err := q.Pop(ctx); popErr <- err }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-popErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pop err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled pop stayed blocked")
	}
}

func TestQueueCloseDrains(t *testing.T) {
	q, _ := NewQueue[int](4)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := q.Push(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	q.Close()
	for i := 0; i < 2; i++ {
		if v, err := q.Pop(ctx); err != nil || v != i {
			t.Fatalf("drain %d: v=%d err=%v", i, v, err)
		}
	}
	if _, err := q.Pop(ctx); err == nil {
		t.Fatal("drained closed queue returned an item")
	}
	if !q.Closed() {
		t.Fatal("Closed()=false")
	}
	// Push on a closed queue fails immediately.
	if err := q.Push(ctx, 5); err == nil {
		t.Fatal("push on closed queue accepted")
	}
}

func TestQueueConcurrentBounded(t *testing.T) {
	q, _ := NewQueue[media.Block](4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	// Slow consumer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, err := q.Pop(ctx); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	// Fast producer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := q.Push(ctx, media.Block{Seq: uint64(i)}); err != nil {
				return
			}
			if q.Len() > q.Cap() {
				t.Errorf("len %d > cap %d", q.Len(), q.Cap())
				return
			}
		}
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	wg.Wait()
	st := q.Stats()
	if st.HighWater > 4 {
		t.Fatalf("high water %d", st.HighWater)
	}
	if st.Backpressure == 0 {
		t.Fatal("no backpressure recorded")
	}
}
