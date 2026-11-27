package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

func testEvent(payload string) model.Event {
	return model.Event{Track: 0, Start: clock.Must(0, 1000), Payload: payload}
}

func TestBackpressureBoundsInFlight(t *testing.T) {
	const capacity = 2
	const total = 6
	q := NewQueue(capacity, NewBudget(model.MaxBufferedBytes))
	ctx := context.Background()

	var sent, received atomic.Int64
	// Slow consumer: 20ms per event.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, ok, err := q.Recv(ctx)
			if err != nil || !ok {
				return
			}
			received.Add(1)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	// Producer records how far it runs ahead of the consumer.
	go func() {
		for i := 0; i < total; i++ {
			if err := q.Send(ctx, testEvent("x")); err != nil {
				return
			}
			sent.Add(1)
		}
	}()
	time.Sleep(50 * time.Millisecond) // consumer is mid-first-event
	lead := sent.Load() - received.Load()
	if lead > int64(capacity)+1 {
		t.Fatalf("producer ran ahead by %d, capacity %d", lead, capacity)
	}
	// Drain: let producer finish, then close and verify all received.
	deadline := time.Now().Add(2 * time.Second)
	for sent.Load() < total && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	q.Close()
	<-done
	if received.Load() != total {
		t.Fatalf("received %d of %d", received.Load(), total)
	}
}

func TestCancelStopsBlockedProducer(t *testing.T) {
	q := NewQueue(1, NewBudget(model.MaxBufferedBytes))
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Send(ctx, testEvent("first")); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- q.Send(ctx, testEvent("blocked")) }()
	time.Sleep(20 * time.Millisecond) // let the producer block on the full queue
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not stop after cancellation")
	}
}

func TestBudgetLimitAndPeak(t *testing.T) {
	b := NewBudget(100)
	if err := b.Add(60); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(50); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if b.Peak() != 110 {
		t.Fatalf("peak = %d, want 110", b.Peak())
	}
	b.Sub(60)
	if err := b.Add(50); err != nil {
		t.Fatal(err)
	}
}

func TestRunPrefersCauseOverCancellation(t *testing.T) {
	cause := errors.New("root cause")
	err := Run(
		func() error { return context.Canceled },
		func() error { return cause },
	)
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want root cause", err)
	}
	if err := Run(func() error { return nil }); err != nil {
		t.Fatalf("err = %v", err)
	}
}
