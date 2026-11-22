package stream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPipeBackpressure(t *testing.T) {
	p := NewPipe[int](2)
	ctx := context.Background()
	if err := p.Send(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.Send(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// Queue full: a slow consumer forces the producer to block.
	done := make(chan error, 1)
	go func() { done <- p.Send(ctx, 3) }()
	select {
	case <-done:
		t.Fatal("send did not block on full queue")
	case <-time.After(50 * time.Millisecond):
	}
	v, ok := p.Recv(ctx)
	if !ok || v != 1 {
		t.Fatalf("recv %d %v", v, ok)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPipeCancelStopsDispatch(t *testing.T) {
	p := NewPipe[int](1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Send(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := p.Send(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v want context.Canceled", err)
	}
	// Already-queued item is still delivered (completed work is kept).
	if v, ok := p.Recv(context.Background()); !ok || v != 1 {
		t.Fatalf("completed item lost: %d %v", v, ok)
	}
}

func TestPipeClose(t *testing.T) {
	p := NewPipe[int](4)
	ctx := context.Background()
	p.Send(ctx, 7)
	p.Close()
	if err := p.Send(ctx, 8); !errors.Is(err, ErrClosed) {
		t.Fatalf("err %v want ErrClosed", err)
	}
	if v, ok := p.Recv(ctx); !ok || v != 7 {
		t.Fatalf("queued item lost: %d %v", v, ok)
	}
	if _, ok := p.Recv(ctx); ok {
		t.Fatal("recv after drain should report closed")
	}
}

func TestBudget(t *testing.T) {
	b := NewBudget(100)
	if err := b.Reserve(60); err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve(50); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err %v want ErrBudgetExceeded", err)
	}
	if b.Peak() != 60 {
		t.Fatalf("peak %d want 60", b.Peak())
	}
	b.Release(30)
	if err := b.Reserve(70); err != nil {
		t.Fatal(err)
	}
	if b.Peak() != 100 {
		t.Fatalf("peak %d want 100", b.Peak())
	}
	b.Release(1000) // over-release clamps to zero
	if err := b.Reserve(100); err != nil {
		t.Fatal(err)
	}
}
