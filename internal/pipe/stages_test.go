package pipe

import (
	"context"
	"errors"
	"testing"
	"time"
)

type blob struct{ n int }

func (b blob) Size() int { return b.n }

func TestBackpressure(t *testing.T) {
	q := NewQueue[blob](2, nil)
	ctx := context.Background()
	q.Push(ctx, blob{1})
	q.Push(ctx, blob{1})
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, blob{1}) }()
	select {
	case <-done:
		t.Fatal("push should block while queue is full")
	case <-time.After(50 * time.Millisecond):
	}
	if _, ok := q.Pop(ctx); !ok {
		t.Fatal("pop failed")
	}
	if err := <-done; err != nil { // slow consumer unblocks producer
		t.Fatalf("push after drain: %v", err)
	}
}

func TestCancelReleasesBlockedPush(t *testing.T) {
	q := NewQueue[blob](1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	q.Push(ctx, blob{1})
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, blob{1}) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestMeterPeakAndBudget(t *testing.T) {
	m := &Meter{}
	if err := m.Add(MaxBufferBytes); err != nil {
		t.Fatal(err)
	}
	if m.Peak != MaxBufferBytes {
		t.Fatalf("peak = %d", m.Peak)
	}
	m.Sub(MaxBufferBytes)
	if err := m.Add(MaxBufferBytes + 1); err == nil {
		t.Fatal("budget overflow should fail")
	}
}

func TestRunCancelsSiblings(t *testing.T) {
	boom := errors.New("boom")
	released := make(chan struct{})
	err := Run(context.Background(),
		func(ctx context.Context) error { return boom },
		func(ctx context.Context) error {
			<-ctx.Done() // must be cancelled by the failing sibling
			close(released)
			return nil
		},
	)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("sibling goroutine was not released")
	}
}

func TestDrainAfterClose(t *testing.T) {
	q := NewQueue[blob](4, nil)
	ctx := context.Background()
	q.Push(ctx, blob{3})
	q.Close()
	if _, ok := q.Pop(ctx); !ok {
		t.Fatal("buffered item lost on close")
	}
	if _, ok := q.Pop(ctx); ok {
		t.Fatal("queue should report end-of-stream")
	}
}
