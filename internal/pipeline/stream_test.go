package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueBoundedAndPeak(t *testing.T) {
	const cap = 4
	q := NewQueue(cap)
	ctx := context.Background()
	// Fill to capacity without a consumer; depth must never exceed cap.
	for i := 0; i < cap; i++ {
		if err := q.Send(ctx, Frame{Index: i}); err != nil {
			t.Fatal(err)
		}
	}
	if got := q.Snapshot().PeakDepth; got != cap {
		t.Fatalf("peak = %d, want %d", got, cap)
	}
	// One more Send must block until a Receive frees a slot.
	done := make(chan error, 1)
	go func() { done <- q.Send(ctx, Frame{Index: 99}) }()
	select {
	case <-done:
		t.Fatal("Send on full queue must block (backpressure)")
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := q.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Send did not resume after Receive")
	}
}

func TestQueueCancellation(t *testing.T) {
	q := NewQueue(1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Send(ctx, Frame{}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := q.Send(ctx, Frame{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRunnerCommitsOnlyCompleteSegments(t *testing.T) {
	q := NewQueue(4)
	r := NewRunner(q)
	ctx, cancel := context.WithCancel(context.Background())

	var gotMu sync.Mutex
	got := make([]FrameKind, 0)
	go func() {
		_ = r.Run(ctx, func(f Frame) error {
			gotMu.Lock()
			got = append(got, f.Kind)
			gotMu.Unlock()
			return nil
		})
	}()

	// Two complete segments, then uncommitted samples, then cancel.
	_ = q.Send(ctx, Frame{Kind: FrameSample, Index: 0})
	_ = q.Send(ctx, Frame{Kind: FrameSegmentCommit, TimeNS: 100, Meta: map[string]string{"fingerprint": "aa"}})
	_ = q.Send(ctx, Frame{Kind: FrameSample, Index: 1})
	_ = q.Send(ctx, Frame{Kind: FrameSegmentCommit, TimeNS: 200, Meta: map[string]string{"fingerprint": "bb"}})
	_ = q.Send(ctx, Frame{Kind: FrameSample, Index: 2})
	_ = q.Send(ctx, Frame{Kind: FrameSample, Index: 3})
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(30 * time.Millisecond)

	if r.Committed() != 2 {
		t.Fatalf("committed = %d, want 2", r.Committed())
	}
	cp := r.Checkpoint()
	if cp.SegmentSeq != 1 || cp.TimeNS != 200 || cp.Fingerprint != "bb" {
		t.Fatalf("checkpoint = %+v", cp)
	}
	if !r.Canceled() {
		t.Fatal("runner must record cancellation")
	}
	// Resume boundary equals the last verified commit.
	if r.ResumeFrom().TimeNS != 200 {
		t.Fatal("resume boundary not at last complete segment")
	}
}

func TestSlowConsumerBackpressuresProducer(t *testing.T) {
	q := NewQueue(2)
	ctx := context.Background()
	var blocked int64
	// Consumer that deliberately stalls, proving producers wait rather than
	// the queue growing without bound.
	go func() {
		for {
			f, err := q.Receive(ctx)
			if err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
			_ = f
		}
	}()
	for i := 0; i < 8; i++ {
		start := time.Now()
		if err := q.Send(ctx, Frame{Index: i}); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 10*time.Millisecond {
			atomic.StoreInt64(&blocked, 1)
		}
	}
	if atomic.LoadInt64(&blocked) == 0 {
		t.Fatal("producer was never blocked by the slow consumer")
	}
	if q.Snapshot().PeakDepth > q.Capacity() {
		t.Fatal("queue exceeded its configured capacity")
	}
}

func TestTrySendDropPolicy(t *testing.T) {
	q := NewQueue(1)
	ctx := context.Background()
	if !q.TrySend(Frame{}) {
		t.Fatal("first TrySend should succeed")
	}
	if q.TrySend(Frame{}) {
		t.Fatal("second TrySend on full queue must report false, not block")
	}
	if q.Snapshot().Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", q.Snapshot().Dropped)
	}
	_, _ = q.Receive(ctx)
}
