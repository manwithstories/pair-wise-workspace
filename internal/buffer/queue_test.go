package buffer

import (
	"context"
	"errors"
	"testing"
	"time"

	"archive/internal/model"
)

const itemBytes = 10

func newTestQueue(capBytes int) (*Queue[int], *model.FaultLog) {
	log := model.NewFaultLog()
	sizer := func(int) int { return itemBytes }
	return NewQueue[int](capBytes, sizer, log), log
}

func TestFIFOAndPeak(t *testing.T) {
	q, _ := newTestQueue(3 * itemBytes)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := q.Push(ctx, i); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if q.Bytes() != 3*itemBytes || q.PeakBytes() != 3*itemBytes {
		t.Fatalf("accounting wrong: bytes=%d peak=%d", q.Bytes(), q.PeakBytes())
	}
	for i := 1; i <= 3; i++ {
		v, err := q.Pop(ctx)
		if err != nil || v != i {
			t.Fatalf("pop %d: v=%d err=%v", i, v, err)
		}
	}
	if q.Bytes() != 0 {
		t.Fatalf("bytes not released: %d", q.Bytes())
	}
}

func TestBackpressureBlocksAndReleases(t *testing.T) {
	q, log := newTestQueue(2 * itemBytes)
	ctx := context.Background()
	_ = q.Push(ctx, 1)
	_ = q.Push(ctx, 2)

	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, 3) }()

	select {
	case <-done:
		t.Fatal("third push must block under the byte cap")
	case <-time.After(50 * time.Millisecond):
	}
	if q.BackpressureEvents() != 1 || log.Count(model.FaultBackpressure) != 1 {
		t.Fatal("backpressure event not recorded")
	}
	if _, err := q.Pop(ctx); err != nil {
		t.Fatalf("pop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked push should resume after pop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked push never resumed")
	}
}

func TestCancelReleasesWaiterAndKeepsQueue(t *testing.T) {
	q, _ := newTestQueue(itemBytes)
	_ = q.Push(context.Background(), 7)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- q.Push(ctx, 8) }()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release the waiter")
	}
	// Existing work must remain determinable after cancellation.
	got, err := q.Pop(context.Background())
	if err != nil || got != 7 {
		t.Fatalf("queue lost buffered work: v=%d err=%v", got, err)
	}
}

func TestCloseDrainsThenErrors(t *testing.T) {
	q, _ := newTestQueue(3 * itemBytes)
	ctx := context.Background()
	_ = q.Push(ctx, 1)
	_ = q.Push(ctx, 2)
	q.Close()

	got := q.DrainAll()
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("DrainAll lost order/content: %v", got)
	}
	if _, err := q.Pop(ctx); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("want ErrQueueClosed after drain, got %v", err)
	}
	if err := q.Push(ctx, 3); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("push after close must fail, got %v", err)
	}
}

func TestOversizeRejected(t *testing.T) {
	log := model.NewFaultLog()
	q := NewQueue[string](5, func(string) int { return 10 }, log)
	err := q.Push(context.Background(), "x")
	if !errors.Is(err, model.ErrInvariant) {
		t.Fatalf("oversize item must be rejected, got %v", err)
	}
	if log.Count(model.FaultOversize) != 1 {
		t.Fatal("oversize fault not recorded")
	}
}
