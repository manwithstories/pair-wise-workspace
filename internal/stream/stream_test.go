package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/clearliu/fieldcut/internal/clock"
)

func TestBoundedQueueByteAndItemCaps(t *testing.T) {
	q := NewBoundedQueue(100, 2)
	ctx := context.Background()
	if err := q.TryEnqueue(Item{TrackID: 0, Sequence: 0, Size: 60}); err != nil {
		t.Fatal(err)
	}
	if err := q.TryEnqueue(Item{TrackID: 0, Sequence: 1, Size: 30}); err != nil {
		t.Fatal(err)
	}
	// Byte budget exhausted (90 + 20 > 100).
	if err := q.TryEnqueue(Item{Size: 20}); !errors.Is(err, clock.ErrQueueFull) {
		t.Fatalf("byte cap: want ErrQueueFull, got %v", err)
	}
	// Drain one; item count cap now binds before bytes matter.
	if _, ok, err := q.Dequeue(ctx); err != nil || !ok {
		t.Fatalf("dequeue: %v %v", ok, err)
	}
	if err := q.TryEnqueue(Item{Size: 10}); err != nil {
		t.Fatalf("after drain enqueue failed: %v", err)
	}
	if err := q.TryEnqueue(Item{Size: 10}); !errors.Is(err, clock.ErrQueueFull) {
		t.Fatalf("item cap: want ErrQueueFull, got %v", err)
	}
	m := q.Snapshot()
	if m.PeakBytes != 90 || m.TotalIn != 3 || m.Dropped != 2 {
		t.Fatalf("metrics wrong: %+v", m)
	}
}

func TestBoundedQueueBackpressure(t *testing.T) {
	q := NewBoundedQueue(50, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.TryEnqueue(Item{Size: 50}); err != nil {
		t.Fatal(err)
	}
	// Enqueue must block while full; run it in a goroutine and prove it
	// only completes after a Dequeue frees budget.
	done := make(chan error, 1)
	go func() { done <- q.Enqueue(ctx, Item{Size: 10}) }()
	select {
	case <-done:
		t.Fatal("Enqueue returned before budget was freed")
	case <-time.After(20 * time.Millisecond):
	}
	if _, ok, _ := q.Dequeue(ctx); !ok {
		t.Fatal("dequeue failed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked enqueue: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue never unblocked after drain")
	}
}

func TestBoundedQueueCancelAndClose(t *testing.T) {
	q := NewBoundedQueue(10, 1)
	ctx, cancel := context.WithCancel(context.Background())
	_ = q.TryEnqueue(Item{Size: 10})
	errCh := make(chan error, 1)
	go func() { errCh <- q.Enqueue(ctx, Item{Size: 1}) }()
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled Enqueue must return context error")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Enqueue never returned")
	}

	// Closed queue rejects enqueues but drains remaining items.
	q.Close()
	if err := q.TryEnqueue(Item{Size: 1}); !errors.Is(err, clock.ErrQueueClosed) {
		t.Fatalf("closed enqueue: %v", err)
	}
	it, ok, err := q.Dequeue(context.Background())
	if err != nil || !ok || it.Size != 10 {
		t.Fatalf("drain after close: %+v %v %v", it, ok, err)
	}
	if _, ok, err := q.Dequeue(context.Background()); err != nil || ok {
		t.Fatalf("empty closed queue must terminate: ok=%v err=%v", ok, err)
	}
}

func TestBoundedQueueDrainReleasesMemory(t *testing.T) {
	q := NewBoundedQueue(1000, 10)
	for i := 0; i < 5; i++ {
		if err := q.TryEnqueue(Item{Sequence: i, Size: 100}); err != nil {
			t.Fatal(err)
		}
	}
	if released := q.DrainAndRelease(); released != 500 {
		t.Fatalf("released %d bytes, want 500", released)
	}
	if m := q.Snapshot(); m.QueuedBytes != 0 || m.QueuedItems != 0 {
		t.Fatalf("queue not empty after drain: %+v", m)
	}
}

func TestBoundedQueueCheckpoint(t *testing.T) {
	q := NewBoundedQueue(1000, 10)
	for i := 0; i < 3; i++ {
		if err := q.TryEnqueue(Item{TrackID: 7, Sequence: i, Size: 100}); err != nil {
			t.Fatal(err)
		}
	}
	cp := q.Checkpoint()
	if cp.QueuedItems != 3 || cp.QueuedBytes != 300 || len(cp.Pending) != 3 {
		t.Fatalf("checkpoint wrong: %+v", cp)
	}
	if err := q.Resume(cp); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}
	bad := cp
	bad.QueuedBytes = 999
	if err := q.Resume(bad); !errors.Is(err, clock.ErrCheckpoint) {
		t.Fatalf("tampered checkpoint: %v", err)
	}
	// Capacity enforcement.
	small := NewBoundedQueue(50, 10)
	if err := small.Resume(cp); !errors.Is(err, clock.ErrCheckpoint) {
		t.Fatalf("oversize checkpoint must fail: %v", err)
	}
}

func TestPipelineBackpressureAndCancel(t *testing.T) {
	root := NewBoundedQueue(40, 4)
	mid := NewBoundedQueue(40, 4)
	var sinkMu sync.Mutex
	var sunk []int
	producer := NewStage(StageConfig{Name: "gen"}, root, mid,
		func(_ context.Context, in Item) ([]Item, error) {
			return []Item{in}, nil
		})
	// Slow consumer: 25ms per item forces root/mid backpressure.
	consumer := NewStage(StageConfig{Name: "sink", ProcessDelay: 25 * time.Millisecond}, mid, nil,
		func(_ context.Context, in Item) ([]Item, error) {
			sinkMu.Lock()
			sunk = append(sunk, in.Sequence)
			sinkMu.Unlock()
			return nil, nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = producer.Run(ctx)
	}()
	go func() {
		_ = consumer.Run(ctx)
	}()
	// Feed 8 items; bounded queues must block the feed rather than grow.
	for i := 0; i < 8; i++ {
		if err := root.Enqueue(ctx, Item{Sequence: i, Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	root.Close()
	// Wait for the sink to finish draining.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sinkMu.Lock()
		n := len(sunk)
		sinkMu.Unlock()
		if n == 8 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	sinkMu.Lock()
	got := len(sunk)
	sinkMu.Unlock()
	if got != 8 {
		t.Fatalf("sink processed %d, want 8", got)
	}
	if root.Snapshot().PeakBytes > 40 || mid.Snapshot().PeakBytes > 40 {
		t.Fatal("peak buffer exceeded hard byte budget")
	}
	if p95 := consumer.LatencyP95(); p95 < 20*time.Millisecond {
		t.Fatalf("p95 latency %v does not reflect slow consumer", p95)
	}
}

func TestPipelinePropagatesStageError(t *testing.T) {
	root := NewBoundedQueue(100, 10)
	sink := NewBoundedQueue(100, 10)
	st := NewStage(StageConfig{Name: "fail"}, root, sink,
		func(_ context.Context, in Item) ([]Item, error) {
			if in.Sequence == 2 {
				return nil, errors.New("boom")
			}
			return []Item{in}, nil
		})
	ctx := context.Background()
	go func() {
		for i := 0; i < 5; i++ {
			_ = root.TryEnqueue(Item{Sequence: i, Size: 10})
		}
	}()
	err := Pipeline(ctx, []*Stage{st}, root, sink)
	if err == nil {
		t.Fatal("stage error must propagate from Pipeline")
	}
	if st.Snapshot().Errors == 0 {
		t.Fatal("error counter not incremented")
	}
}
