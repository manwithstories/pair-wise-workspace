package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/relayteam/relay/internal/media"
)

func TestQueueNeverExceedsByteLimitUnderSlowConsumer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q, err := NewQueue[int](128, 4096)
	if err != nil {
		t.Fatal(err)
	}
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := 0; i < 80; i++ {
			if err := q.Push(ctx, i, 1024); err != nil {
				return
			}
		}
	}()

	received := 0
	for received < 80 {
		_, w, err := q.Pop(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := SlowHook(ctx, int64(received+1), 3, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		q.Release(w)
		received++
		if q.Stats().PeakBytes > 4096 {
			t.Fatalf("peak=%d exceeds limit", q.Stats().PeakBytes)
		}
	}
	<-producerDone
	if got := q.Stats().PeakBytes; got > 4096 || got <= 0 {
		t.Fatalf("peak=%d, want 1..4096", got)
	}
	if q.Stats().WaitCount == 0 {
		t.Fatal("slow consumer should produce at least one backpressure wait")
	}
}

func TestQueueRejectsOversizedItem(t *testing.T) {
	q, err := NewQueue[int](4, 128)
	if err != nil {
		t.Fatal(err)
	}
	err = q.Push(context.Background(), 1, 4096)
	if !errors.Is(err, media.ErrArgument) {
		t.Fatalf("err=%v, want argument error", err)
	}
}
