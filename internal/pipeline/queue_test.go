package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mediacheck/internal/model"
)

func pkt(n int) model.Packet {
	return model.Packet{Track: model.TrackAudio, Sequence: uint32(n), Data: make([]byte, 10)}
}

func TestQueuePreservesOrder(t *testing.T) {
	q := NewQueue(4, 1<<20)
	for i := 0; i < 3; i++ {
		if err := q.Push(pkt(i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		p, ok, err := q.Pop()
		if err != nil || !ok {
			t.Fatalf("Pop %d: ok=%v err=%v", i, ok, err)
		}
		if p.Sequence != uint32(i) {
			t.Fatalf("Pop %d = seq %d", i, p.Sequence)
		}
	}
	q.Close()
	if _, ok, err := q.Pop(); ok || err != nil {
		t.Fatalf("Pop after drain: ok=%v err=%v", ok, err)
	}
}

func TestQueueBlocksProducerWhenFull(t *testing.T) {
	q := NewQueue(2, 1<<20)
	if err := q.Push(pkt(0)); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(pkt(1)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Push(pkt(2)) }()
	select {
	case <-done:
		t.Fatal("third push returned while queue was full")
	case <-time.After(50 * time.Millisecond):
	}
	if got := q.Stats().PeakItems; got != 2 {
		t.Fatalf("peak = %d, want 2", got)
	}
	if _, _, err := q.Pop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked push returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("push did not resume after a pop")
	}
}

func TestQueueByteBudgetBlocksProducer(t *testing.T) {
	q := NewQueue(100, 25)
	for i := 0; i < 2; i++ {
		if err := q.Push(pkt(i)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- q.Push(pkt(2)) }()
	select {
	case <-done:
		t.Fatal("push ignored the byte budget")
	case <-time.After(50 * time.Millisecond):
	}
	if got := q.Stats().PeakBytes; got != 20 {
		t.Fatalf("peak bytes = %d, want 20", got)
	}
	q.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("blocked push = %v, want ErrQueueClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release the blocked producer")
	}
}

func TestQueueCancelReleasesBlockedPop(t *testing.T) {
	q := NewQueue(2, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	q.Bind(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, err := q.Pop()
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("empty pop returned before cancel")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, model.ErrCanceled) {
			t.Fatalf("pop after cancel = %v, want ErrCanceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release the blocked consumer")
	}
}

func TestQueueCancelReleasesBlockedPush(t *testing.T) {
	q := NewQueue(1, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	q.Bind(ctx)
	if err := q.Push(pkt(0)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- q.Push(pkt(1)) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, model.ErrCanceled) {
			t.Fatalf("push after cancel = %v, want ErrCanceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release the blocked producer")
	}
}

func TestQueuePeakNeverExceedsBound(t *testing.T) {
	const bound = 8
	q := NewQueue(bound, 1<<20)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if err := q.Push(pkt(i)); err != nil {
				return
			}
		}
		q.Close()
	}()
	go func() {
		defer wg.Done()
		for {
			_, ok, err := q.Pop()
			if err != nil || !ok {
				return
			}
		}
	}()
	wg.Wait()
	st := q.Stats()
	if st.PeakItems > bound {
		t.Fatalf("peak %d exceeded bound %d", st.PeakItems, bound)
	}
	if st.Popped != 500 {
		t.Fatalf("popped %d, want 500", st.Popped)
	}
	if !q.WithinBounds() {
		t.Fatal("WithinBounds false")
	}
}

func TestQueueStatsAfterClose(t *testing.T) {
	q := NewQueue(3, 1<<20)
	q.Push(pkt(0))
	q.Close()
	if err := q.Push(pkt(1)); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("push after close = %v", err)
	}
	if p, ok, err := q.Pop(); err != nil || !ok || p.Sequence != 0 {
		t.Fatal("queued item should survive Close for draining")
	}
	st := q.Stats()
	if st.Pushed != 1 || st.Popped != 1 || st.MaxItems != 3 {
		t.Fatalf("stats = %+v", st)
	}
}
