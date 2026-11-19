package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

func msItem(seq int, bytes int64, durMs float64) Item {
	start := clock.FromMillis(float64(seq) * 10)
	return Item{Seq: int64(seq), Bytes: bytes, Span: clock.Span{Start: start, End: start + clock.FromMillis(durMs)}}
}

func mustQueue(t *testing.T, cfg Config) *Queue {
	t.Helper()
	q, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQueueRejectsUnboundedConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("unbounded queue accepted")
	}
	if _, err := New(Config{MaxBytes: 1, Policy: "explode"}); err == nil {
		t.Fatal("unknown policy accepted")
	}
}

func TestQueueByteLimitBlocksAndWakesOnPop(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 100, Policy: Block})
	if _, err := q.Push(msItem(0, 60, 10)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := q.Push(msItem(1, 60, 10))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("second push did not block: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, ok := q.Pop(); !ok {
		t.Fatal("pop failed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked push woke with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pop did not wake blocked producer")
	}
	if q.Stats().PeakBytes > 100 {
		t.Fatalf("peak bytes %d exceeds cap", q.Stats().PeakBytes)
	}
}

func TestQueueDurationLimit(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 1 << 20, MaxTicks: int64(clock.FromMillis(15)), Policy: Block})
	if _, err := q.Push(msItem(0, 1, 10)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		q.Push(msItem(1, 1, 10))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("duration limit not enforced")
	case <-time.After(50 * time.Millisecond):
	}
	q.Close(nil)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wake duration-blocked producer")
	}
}

func TestQueueDropPolicyRejects(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 100, Policy: Drop})
	q.Push(msItem(0, 80, 10))
	_, err := q.Push(msItem(1, 80, 10))
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeQueueLimit {
		t.Fatalf("want queue limit, got %v", err)
	}
	if q.Stats().Dropped != 1 {
		t.Fatalf("dropped=%d", q.Stats().Dropped)
	}
}

func TestQueueDropOldestEvicts(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 100, Policy: DropOldest})
	q.Push(msItem(0, 60, 10))
	evicted, err := q.Push(msItem(1, 60, 10))
	if err != nil {
		t.Fatal(err)
	}
	if evicted == nil || evicted.Seq != 1 {
		t.Fatalf("evicted=%+v", evicted)
	}
	if q.Bytes() != 60 {
		t.Fatalf("bytes=%d", q.Bytes())
	}
	if q.Stats().Evicted != 1 {
		t.Fatalf("evicted count=%d", q.Stats().Evicted)
	}
}

func TestQueueOversizedItemRejectedByDropOldest(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 100, Policy: DropOldest})
	_, err := q.Push(msItem(0, 200, 10))
	if e := fault.Root(err); e == nil || e.Code != fault.CodeQueueLimit {
		t.Fatalf("want queue limit, got %v", err)
	}
}

func TestCloseWakesBlockedProducer(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 10, Policy: Block})
	q.Push(msItem(0, 10, 10))
	errc := make(chan error, 1)
	go func() {
		_, err := q.Push(msItem(1, 10, 10))
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	q.Close(nil)
	select {
	case err := <-errc:
		if e := fault.Root(err); e == nil || e.Code != fault.CodeQueueClosed {
			t.Fatalf("want queue closed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wake producer")
	}
}

func TestCancelWakesBlockedProducerWithCancel(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 10, Policy: Block})
	q.Push(msItem(0, 10, 10))
	errc := make(chan error, 1)
	go func() {
		_, err := q.Push(msItem(1, 10, 10))
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	q.Cancel(context.Canceled)
	err := <-errc
	root := fault.Root(err)
	if root == nil || root.Code != fault.CodeQueueCanceled {
		t.Fatalf("want cancel root cause, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel cause lost: %v", err)
	}
	if fault.ExitCode(err) != 4 {
		t.Fatalf("cancel exit=%d want 4", fault.ExitCode(err))
	}
}

func TestConsumerDrainsAfterCloseAndExits(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 100, Policy: Block})
	q.Push(msItem(0, 10, 10))
	q.Push(msItem(1, 10, 10))
	q.Close(nil)
	var got []int64
	for {
		it, ok := q.Pop()
		if !ok {
			break
		}
		got = append(got, it.Seq)
	}
	if len(got) != 2 {
		t.Fatalf("drained %v", got)
	}
}

func TestPopBlocksUntilItemOrClose(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 100})
	got := make(chan Item, 1)
	go func() {
		it, ok := q.Pop()
		if ok {
			got <- it
		}
	}()
	time.Sleep(20 * time.Millisecond)
	q.Push(msItem(0, 10, 10))
	select {
	case it := <-got:
		if it.Bytes != 10 {
			t.Fatalf("item=%+v", it)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer not woken by push")
	}
}

func TestDrainReleasesBufferAndWakesProducers(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 100, Policy: Block})
	q.Push(msItem(0, 60, 10))
	q.Push(msItem(1, 40, 10))
	done := make(chan struct{})
	go func() {
		q.Push(msItem(2, 50, 10))
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	items := q.Drain()
	if len(items) != 2 {
		t.Fatalf("drained %d", len(items))
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not wake producer")
	}
	if q.Bytes() != 50 {
		t.Fatalf("bytes=%d", q.Bytes())
	}
}

func TestConsumerExitWithQueuedItemsReleasesMemory(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 1000, Policy: Block})
	for i := 0; i < 5; i++ {
		q.Push(msItem(i, 100, 10))
	}
	q.Pop()
	q.Close(nil)
	q.Drain()
	if q.Bytes() != 0 || q.Len() != 0 {
		t.Fatalf("memory not released: bytes=%d len=%d", q.Bytes(), q.Len())
	}
}

func TestQueueConcurrentStress(t *testing.T) {
	q := mustQueue(t, Config{MaxBytes: 500, MaxTicks: int64(clock.FromMillis(50)), Policy: Block})
	var wg sync.WaitGroup
	var consumed int64
	var mu sync.Mutex
	for p := 0; p < 3; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := q.Push(msItem(p*100+i, 20, 1)); err != nil {
					return
				}
			}
		}(p)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 150; i++ {
			if _, ok := q.Pop(); !ok {
				return
			}
			mu.Lock()
			consumed++
			mu.Unlock()
		}
	}()
	time.Sleep(30 * time.Millisecond)
	q.Close(nil)
	wg.Wait()
	mu.Lock()
	got := consumed
	mu.Unlock()
	if got != 150 {
		t.Fatalf("consumed %d of 150", got)
	}
	if st := q.Stats(); st.PeakBytes > 500 || st.PeakTicks > int64(clock.FromMillis(50)) {
		t.Fatalf("limits exceeded: %+v", st)
	}
}
