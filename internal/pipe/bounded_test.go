package pipe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestQueueNeverExceedsCapacity(t *testing.T) {
	q := NewQueue[int](3)
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := q.Send(ctx, i); err != nil {
				return
			}
		}
		q.Close()
	}()
	time.Sleep(5 * time.Millisecond)
	if q.Len() > 3 {
		t.Fatalf("occupancy %d exceeds capacity 3", q.Len())
	}
	q.Close()
	wg.Wait()
	if q.Peak() > 3 {
		t.Fatalf("peak %d exceeds capacity", q.Peak())
	}
}

func TestSendBlocksUntilConsumerDrains(t *testing.T) {
	q := NewQueue[int](1)
	ctx := context.Background()
	if err := q.Send(ctx, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Send(ctx, 2) }()
	select {
	case <-done:
		t.Fatal("second send did not block on a full queue")
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := q.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked send was not released")
	}
}

func TestSendHonoursCancellation(t *testing.T) {
	q := NewQueue[int](1)
	if err := q.Send(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Send(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestReceiveClosed(t *testing.T) {
	q := NewQueue[int](1)
	q.Close()
	if _, err := q.Receive(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if err := q.Send(context.Background(), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("send on closed queue err = %v", err)
	}
}
