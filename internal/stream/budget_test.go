package stream

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mediapipe/internal/media"
)

func TestPushPopPreservesOrder(t *testing.T) {
	q := NewQueue[int](1024, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := q.Push(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		got, err := q.Pop(ctx)
		if err != nil || got != i {
			t.Fatalf("pop %d got=%d err=%v", i, got, err)
		}
	}
}

func TestByteAccountingAndPeak(t *testing.T) {
	q := NewQueue[media.Packet](1000, PacketSizer(0))
	ctx := context.Background()
	pkt := media.Packet{Data: make([]byte, 300)}
	for i := 0; i < 3; i++ {
		if err := q.Push(ctx, pkt); err != nil {
			t.Fatal(err)
		}
	}
	st := q.Stats()
	if st.Bytes != 900 || st.Peak != 900 || st.Items != 3 {
		t.Fatalf("stats=%+v", st)
	}
	if _, err := q.Pop(ctx); err != nil {
		t.Fatal(err)
	}
	if st := q.Stats(); st.Bytes != 600 || st.Peak != 900 {
		t.Fatalf("after pop stats=%+v", st)
	}
}

func TestProducerBlocksAtBudget(t *testing.T) {
	q := NewQueue[[]byte](500, func(b []byte) int64 { return int64(len(b)) })
	ctx := context.Background()
	if err := q.Push(ctx, make([]byte, 300)); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(ctx, make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	// Third push must block: start it in a goroutine and verify it does not
	// complete before a pop frees room.
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, make([]byte, 100)) }()
	select {
	case err := <-done:
		t.Fatalf("push did not block (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := q.Pop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("push after pop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("push never unblocked")
	}
	if peak := q.Peak(); peak > 500 {
		t.Fatalf("peak=%d exceeds budget", peak)
	}
}

func TestCancelReleasesBlockedProducer(t *testing.T) {
	q := NewQueue[[]byte](100, func(b []byte) int64 { return int64(len(b)) })
	if err := q.Push(context.Background(), make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Push(ctx, make([]byte, 100)) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked push ignored cancellation")
	}
}

func TestCancelDropsBufferedItems(t *testing.T) {
	q := NewQueue[int](100, nil)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := q.Push(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	q.Cancel()
	if st := q.Stats(); st.Bytes != 0 || st.Items != 0 || st.Dropped != 10 {
		t.Fatalf("after cancel stats=%+v", st)
	}
	if _, err := q.Pop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pop after cancel err=%v", err)
	}
	if err := q.Push(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("push after cancel err=%v", err)
	}
}

func TestCancelUnblocksConsumer(t *testing.T) {
	q := NewQueue[int](10, nil)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := q.Pop(ctx)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	q.Cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("consumer not released")
	}
}

func TestCloseDrainsThenEOF(t *testing.T) {
	q := NewQueue[int](10, nil)
	ctx := context.Background()
	q.Push(ctx, 1)
	q.Push(ctx, 2)
	q.Close()
	if err := q.Push(ctx, 3); !errors.Is(err, ErrClosed) {
		t.Fatalf("push after close err=%v", err)
	}
	for _, want := range []int{1, 2} {
		got, err := q.Pop(ctx)
		if err != nil || got != want {
			t.Fatalf("drain got=%d err=%v", got, err)
		}
	}
	if _, err := q.Pop(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("pop after drain err=%v", err)
	}
}

func TestOversizeItemRejected(t *testing.T) {
	q := NewQueue[[]byte](64, func(b []byte) int64 { return int64(len(b)) })
	err := q.Push(context.Background(), make([]byte, 65))
	if err == nil || !errors.Is(err, ErrOversize) {
		t.Fatalf("err=%v", err)
	}
	if media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("class=%v", media.ClassOf(err))
	}
}

// TestPipelineNeverExceedsBudget runs a producer and a deliberately slow
// consumer and checks both the budget and that bytes stay bounded.
func TestPipelineNeverExceedsBudget(t *testing.T) {
	const budget = 32 << 10
	q := NewQueue[media.Packet](budget, PacketSizer(16))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var produced, consumed int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer q.Close()
		for i := 0; i < 400; i++ {
			p := media.Packet{Data: make([]byte, 512)}
			if err := q.Push(ctx, p); err != nil {
				return
			}
			atomic.AddInt64(&produced, 1)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, err := q.Pop(ctx)
			if err != nil {
				return
			}
			atomic.AddInt64(&consumed, 1)
			if consumed%7 == 0 {
				time.Sleep(200 * time.Microsecond) // slow consumer
			}
			if st := q.Stats(); st.Bytes > budget {
				t.Errorf("bytes %d exceeded budget %d", st.Bytes, budget)
				return
			}
		}
	}()
	wg.Wait()
	if peak := q.Peak(); peak > budget {
		t.Fatalf("peak %d exceeded budget %d", peak, budget)
	}
	if consumed == 0 {
		t.Fatal("consumer made no progress")
	}
}

func TestConcurrentPushesMaintainAccounting(t *testing.T) {
	q := NewQueue[int](1000, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if err := q.Push(ctx, i); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 800; i++ {
			if _, err := q.Pop(ctx); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	st := q.Stats()
	if st.Items != 0 || st.Bytes != 0 {
		t.Fatalf("stats not drained: %+v", st)
	}
	if st.Pushes != 800 || st.Pops != 800 {
		t.Fatalf("pushes=%d pops=%d", st.Pushes, st.Pops)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		q := NewQueue[int](4, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_, _ = q.Pop(ctx)
			close(done)
		}()
		cancel()
		<-done
		q.Cancel()
	}
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutines grew %d -> %d", before, after)
	}
}

func BenchmarkPushPop(b *testing.B) {
	q := NewQueue[[]byte](1<<20, func(v []byte) int64 { return int64(len(v)) })
	ctx := context.Background()
	item := make([]byte, 1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := q.Push(ctx, item); err != nil {
			b.Fatal(err)
		}
		if _, err := q.Pop(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
