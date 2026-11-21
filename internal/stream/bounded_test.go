package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"archivecheck.local/archivecheck/internal/fault"
)

func TestNewRejectsUnbounded(t *testing.T) {
	if _, err := New[int](0); err == nil {
		t.Fatal("zero capacity must be rejected")
	}
	if _, err := New[int](-1); err == nil {
		t.Fatal("negative capacity must be rejected")
	}
}

func TestFIFOAndDrainAfterClose(t *testing.T) {
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
	q.Close()
	for want := 0; want < 2; want++ {
		got, ok := q.Recv()
		if !ok || got != want {
			t.Fatalf("got %d,%v want %d", got, ok, want)
		}
	}
	if _, ok := q.Recv(); ok {
		t.Fatal("queue should be drained")
	}
}

func TestSendBlocksAtCapacity(t *testing.T) {
	q, err := New[int](1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := q.Send(ctx, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Send(ctx, 2) }()
	select {
	case err := <-done:
		t.Fatalf("send did not block: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got, _ := q.Recv(); got != 1 {
		t.Fatalf("got %d", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("send never unblocked")
	}
	if got, _ := q.Recv(); got != 2 {
		t.Fatalf("got %d", got)
	}
	st := q.Stats()
	if st.Peak != 1 || st.Capacity != 1 || st.Blocked == 0 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestSendUnblocksOnCancel(t *testing.T) {
	q, err := New[int](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Send(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Send(ctx, 2) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		if fault.StageOf(err) != fault.StageQueue {
			t.Fatalf("stage=%q", fault.StageOf(err))
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled send did not return")
	}
}

func TestSendUnblocksOnCloseWithCause(t *testing.T) {
	q, err := New[int](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Send(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Send(context.Background(), 2) }()
	time.Sleep(20 * time.Millisecond)
	cause := errors.New("consumer failed")
	q.CloseWith(cause)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send should fail after close")
		}
	case <-time.After(time.Second):
		t.Fatal("closed send did not return")
	}
}

func TestRecvCtxCancelClosesQueue(t *testing.T) {
	q, err := New[int](2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.RecvCtx(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-q.Done():
	default:
		t.Fatal("queue should be closed after recv cancellation")
	}
}

func TestConcurrentBounded(t *testing.T) {
	const capacity = 4
	q, err := New[int](capacity)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const total = 1000
	go func() {
		for i := 0; i < total; i++ {
			if err := q.Send(ctx, i); err != nil {
				return
			}
		}
		q.Close()
	}()
	seen := 0
	for {
		v, ok := q.Recv()
		if !ok {
			break
		}
		if v != seen {
			t.Fatalf("out of order: got %d want %d", v, seen)
		}
		seen++
		if st := q.Stats(); st.Peak > capacity {
			t.Fatalf("peak %d exceeded capacity %d", st.Peak, capacity)
		}
	}
	if seen != total {
		t.Fatalf("received %d want %d", seen, total)
	}
}
