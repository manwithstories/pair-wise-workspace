package pipe

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"medpipe/internal/clock"
)

func TestQueueBackpressureBlocks(t *testing.T) {
	q, _ := NewQueue(100)
	ctx := context.Background()
	// First item of 80 bytes fits.
	if err := q.Put(ctx, &Item{ID: "a", Bytes: 80}); err != nil {
		t.Fatal(err)
	}
	// Another 80 would exceed capacity; Put must block until Get frees space.
	done := make(chan error, 1)
	go func() {
		done <- q.Put(ctx, &Item{ID: "b", Bytes: 80})
	}()
	select {
	case err := <-done:
		t.Fatalf("Put did not block: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	got, ok, err := q.Get(ctx)
	if err != nil || !ok || got.ID != "a" {
		t.Fatalf("Get wrong: %v %v %v", got, ok, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked Put failed after drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Put never unblocked after consumer drained")
	}
	if q.Inflight() != 80 {
		t.Fatalf("inflight = %d, want 80", q.Inflight())
	}
}

func TestQueueOversizeRejected(t *testing.T) {
	q, _ := NewQueue(10)
	err := q.Put(context.Background(), &Item{ID: "big", Bytes: 11})
	if !clock.IsKind(err, clock.KindArgument) {
		t.Fatalf("want argument error for oversize item, got %v", err)
	}
}

func TestQueueCancelUnblocks(t *testing.T) {
	q, _ := NewQueue(100)
	ctx, cancel := context.WithCancel(context.Background())
	_ = q.Put(context.Background(), &Item{ID: "x", Bytes: 100})

	putDone := make(chan error, 1)
	go func() { putDone <- q.Put(ctx, &Item{ID: "y", Bytes: 10}) }()
	getDone := make(chan error, 1)
	emptyQ, _ := NewQueue(100)
	go func() {
		_, _, err := emptyQ.Get(ctx)
		getDone <- err
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-putDone:
		if !clock.IsKind(err, clock.KindCanceled) {
			t.Fatalf("Put cancel wrong kind: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Put not unblocked by cancel")
	}
	select {
	case err := <-getDone:
		if !clock.IsKind(err, clock.KindCanceled) {
			t.Fatalf("Get cancel wrong kind: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Get not unblocked by cancel")
	}
}

func TestQueueCloseDrain(t *testing.T) {
	q, _ := NewQueue(100)
	_ = q.Put(context.Background(), &Item{ID: "a", Bytes: 1})
	_ = q.Put(context.Background(), &Item{ID: "b", Bytes: 1})
	q.Close()
	// Items already enqueued must still be delivered, then ok goes false.
	for i := 0; i < 2; i++ {
		if _, ok, _ := q.Get(context.Background()); !ok {
			t.Fatalf("closed queue lost queued item %d", i)
		}
	}
	if _, ok, _ := q.Get(context.Background()); ok {
		t.Fatal("drained closed queue still returns items")
	}
}

func TestRunnerSlowConsumerBounded(t *testing.T) {
	const budget = 4096
	var sinkBytes int64

	stages := []Stage{
		{Name: "produce", Workers: 1, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
			return []*Item{{ID: in.ID, Bytes: 512, Data: in.Data}}, nil
		}},
		{Name: "slow", Workers: 1, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
			time.Sleep(2 * time.Millisecond) // deliberately slow consumer
			return []*Item{{ID: in.ID, Bytes: 512}}, nil
		}},
		{Name: "sink", Workers: 1, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
			atomic.AddInt64(&sinkBytes, in.Bytes)
			return nil, nil
		}},
	}
	r, err := NewRunner(budget, 1000, stages)
	if err != nil {
		t.Fatal(err)
	}
	seeds := make([]*Item, 200) // 200 * 512 = 100 KiB, far over the 4 KiB budget
	for i := range seeds {
		seeds[i] = &Item{ID: "seed", Bytes: 512, Data: i}
	}
	metrics, err := r.Run(context.Background(), seeds)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Processed != len(seeds) {
		t.Fatalf("processed %d, want %d", metrics.Processed, len(seeds))
	}
	if metrics.PeakInflight > budget {
		t.Fatalf("peak in-flight %d exceeds budget %d", metrics.PeakInflight, budget)
	}
	if sinkBytes != int64(len(seeds))*512 {
		t.Fatalf("sink bytes = %d, want %d", sinkBytes, int64(len(seeds))*512)
	}
}

func TestRunnerWriteInterruption(t *testing.T) {
	stopErr := errors.New("disk interrupted")
	stages := []Stage{
		{Name: "pass", Workers: 1, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
			return []*Item{in}, nil
		}},
		{Name: "write", Workers: 1, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
			return nil, stopErr // simulate a failed atomic write
		}},
	}
	r, _ := NewRunner(1024, 10, stages)
	seeds := []*Item{{ID: "a", Bytes: 16}, {ID: "b", Bytes: 16}}
	metrics, err := r.Run(context.Background(), seeds)
	if !errors.Is(err, stopErr) {
		t.Fatalf("want interruption error propagated, got %v", err)
	}
	if metrics.Failures != 1 {
		t.Fatalf("failures = %d, want 1", metrics.Failures)
	}
}

func TestRunnerCancellation(t *testing.T) {
	stages := []Stage{
		{Name: "block", Workers: 1, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return []*Item{in}, nil
			}
		}},
	}
	r, _ := NewRunner(1024, 10, stages)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	seeds := make([]*Item, 5)
	for i := range seeds {
		seeds[i] = &Item{ID: "s", Bytes: 8}
	}
	_, err := r.Run(ctx, seeds)
	if !clock.IsKind(err, clock.KindCanceled) && !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestRunnerMemoryIndependentOfDuration(t *testing.T) {
	// Same per-item bytes, 10x more items: peak in-flight must not grow.
	build := func(n int) (int64, error) {
		stages := []Stage{
			{Name: "a", Workers: 2, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
				return []*Item{{ID: in.ID, Bytes: 256}}, nil
			}},
			{Name: "b", Workers: 1, Run: func(ctx context.Context, in *Item) ([]*Item, error) {
				time.Sleep(time.Millisecond)
				return []*Item{{ID: in.ID, Bytes: 256}}, nil
			}},
		}
		r, _ := NewRunner(2048, n, stages)
		seeds := make([]*Item, n)
		for i := range seeds {
			seeds[i] = &Item{ID: "s", Bytes: 256}
		}
		m, err := r.Run(context.Background(), seeds)
		return m.PeakInflight, err
	}
	small, err := build(50)
	if err != nil {
		t.Fatal(err)
	}
	large, err := build(500)
	if err != nil {
		t.Fatal(err)
	}
	if large > small || large > 2048 {
		t.Fatalf("memory grew with input: small peak %d, large peak %d", small, large)
	}
}

func TestRunnerInvalidConfig(t *testing.T) {
	if _, err := NewRunner(10, 1, nil); err == nil {
		t.Fatal("no-stage runner accepted")
	}
	if _, err := NewRunner(10, 0, []Stage{{Name: "x", Workers: 1, Run: nil}}); err == nil {
		t.Fatal("zero maxItems accepted")
	}
	if _, err := NewRunner(10, 1, []Stage{{Name: "x", Workers: 0}}); err == nil {
		t.Fatal("zero-worker stage accepted")
	}
}

func TestRunnerEnforcesMaxConcurrency(t *testing.T) {
	old := MaxConcurrency
	MaxConcurrency = 4
	defer func() { MaxConcurrency = old }()
	noop := func(ctx context.Context, in *Item) ([]*Item, error) { return nil, nil }
	// 3 + 2 = 5 workers exceeds the temporary ceiling of 4.
	over := []Stage{
		{Name: "a", Workers: 3, Run: noop},
		{Name: "b", Workers: 2, Run: noop},
	}
	if _, err := NewRunner(1024, 10, over); !clock.IsKind(err, clock.KindArgument) {
		t.Fatalf("over-limit concurrency not rejected: %v", err)
	}
	// 2 + 2 = 4 is exactly at the ceiling and must be accepted.
	within := []Stage{
		{Name: "a", Workers: 2, Run: noop},
		{Name: "b", Workers: 2, Run: noop},
	}
	if _, err := NewRunner(1024, 10, within); err != nil {
		t.Fatalf("at-ceiling concurrency rejected: %v", err)
	}
}
