package stream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSlowConsumerBackpressure(t *testing.T) {
	const cap, n = 4, 40
	q := NewQueue[int](cap, 0)
	ctx := context.Background()
	done := make(chan int, 1)
	go func() {
		sum := 0
		for {
			v, ok := q.Pop()
			if !ok {
				break
			}
			sum += v
			time.Sleep(time.Millisecond) // slow consumer
		}
		done <- sum
	}()
	for i := 0; i < n; i++ {
		if err := q.Push(ctx, i, 1); err != nil {
			t.Fatalf("push: %v", err)
		}
	}
	q.Close()
	if got := <-done; got != n*(n-1)/2 {
		t.Fatalf("sum=%d", got)
	}
	if q.Peak() > cap {
		t.Fatalf("peak %d exceeds capacity %d", q.Peak(), cap)
	}
	if q.BackpressureEvents() == 0 {
		t.Fatal("expected backpressure with a slow consumer")
	}
}

func TestCancelStopsProducerKeepsCompleted(t *testing.T) {
	q := NewQueue[int](2, 0)
	ctx, cancel := context.WithCancel(context.Background())
	processed := make([]int, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			if err := q.Push(ctx, i, 1); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 3; i++ {
		v, ok := q.Pop()
		if !ok {
			t.Fatal("queue closed early")
		}
		processed = append(processed, v)
	}
	cancel()
	<-done
	if len(processed) != 3 {
		t.Fatalf("processed=%v", processed)
	}
}

func TestSampleTooLarge(t *testing.T) {
	q := NewQueue[int](1, 10)
	if err := q.Push(context.Background(), 1, 11); !errors.Is(err, ErrSampleTooLarge) {
		t.Fatalf("err=%v", err)
	}
	if err := q.Push(context.Background(), 1, 10); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestClosedQueueRejectsPush(t *testing.T) {
	q := NewQueue[int](1, 0)
	q.Close()
	if err := q.Push(context.Background(), 1, 1); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := q.Pop(); ok {
		t.Fatal("pop on drained closed queue")
	}
}

func TestPipelineRunPropagatesErrors(t *testing.T) {
	p := NewPipeline(2, 0)
	q := NewQueue[int](2, 0)
	boom := errors.New("boom")
	err := p.Run(context.Background(), q,
		func(ctx context.Context) error { return nil },
		func(ctx context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}
