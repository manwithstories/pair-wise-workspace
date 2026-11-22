package stream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func sizeOf(v int) int { return v }

func TestQueueRespectsByteBudget(t *testing.T) {
	// 100-item channel but only a 10-byte budget: bytes are the binding
	// constraint here, exercising the condition-variable backpressure.
	q, err := NewQueue[int](100, 10, sizeOf)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := q.Put(ctx, 1); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	// 11th byte cannot fit until a Take happens.
	done := make(chan error, 1)
	go func() { done <- q.Put(ctx, 1) }()
	select {
	case e := <-done:
		t.Fatalf("producer should block when budget full, got %v", e)
	case <-time.After(20 * time.Millisecond):
	}
	if _, ok, err := q.Take(ctx); err != nil || !ok {
		t.Fatalf("take failed: %v %v", ok, err)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("blocked put should resolve after take: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("producer never unblocked after take")
	}
}

func TestQueueStopReleasesProducer(t *testing.T) {
	q, _ := NewQueue[int](1, 10, sizeOf)
	ctx := context.Background()
	_ = q.Put(ctx, 10) // fill budget
	errCh := make(chan error, 1)
	go func() { errCh <- q.Put(ctx, 1) }()
	time.Sleep(10 * time.Millisecond)
	q.Stop()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrStageStopped) {
			t.Fatalf("want ErrStageStopped, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not release the blocked producer")
	}
}

func TestQueueCloseDrainsBuffered(t *testing.T) {
	q, _ := NewQueue[int](4, 100, sizeOf)
	ctx := context.Background()
	for _, v := range []int{1, 2, 3} {
		if err := q.Put(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	q.Close()
	var got []int
	for {
		v, ok, err := q.Take(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, v)
	}
	if len(got) != 3 {
		t.Fatalf("close must retain buffered items, got %v", got)
	}
}

func TestOversizedItemRejected(t *testing.T) {
	q, _ := NewQueue[int](10, 5, sizeOf)
	if err := q.Put(context.Background(), 6); err == nil {
		t.Fatal("item exceeding byte budget must be rejected")
	}
}
