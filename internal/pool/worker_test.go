package pool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessesAllJobs(t *testing.T) {
	ctx := context.Background()
	var done int64
	p := New(ctx, 4, 8, func(ctx context.Context, j Job) (any, error) {
		atomic.AddInt64(&done, 1)
		return j.InputSeq * 2, nil
	})
	const n = 50
	go func() {
		for i := 0; i < n; i++ {
			if err := p.Submit(ctx, Job{InputSeq: i, TrackID: 1}); err != nil {
				t.Errorf("submit: %v", err)
				return
			}
		}
		p.Close()
	}()
	got := 0
	for r := range p.Results() {
		if r.Err != nil {
			t.Fatalf("unexpected err: %v", r.Err)
		}
		got++
		if got == n {
			break
		}
	}
	if atomic.LoadInt64(&done) != n {
		t.Fatalf("processed %d, want %d", done, n)
	}
	if !p.Shutdown(time.Second) {
		t.Fatal("workers did not exit within timeout")
	}
}

func TestCancelPropagatesAndExitsFast(t *testing.T) {
	// Capacity 1 and a blocked producer fill the queue so cancellation must
	// still unwind producer, workers and consumer promptly.
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{}, 16)
	p := New(ctx, 2, 1, func(ctx context.Context, j Job) (any, error) {
		started <- struct{}{}
		<-release // block until released or context cancels
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return j.InputSeq, nil
		}
	})
	go func() {
		for i := 0; i < 100000; i++ {
			if err := p.Submit(ctx, Job{InputSeq: i}); err != nil {
				return // expected once cancelled
			}
		}
	}()
	<-started // ensure at least one worker picked up a job
	cancel()
	close(release)
	if !p.Shutdown(2 * time.Second) {
		t.Fatal("goroutines did not exit within bounded time after cancel")
	}
}

func TestSubmitAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := New(ctx, 1, 2, func(ctx context.Context, j Job) (any, error) {
		return nil, nil
	})
	cancel()
	if err := p.Submit(ctx, Job{InputSeq: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	p.Shutdown(time.Second)
}

func TestBackpressureBoundedQueue(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	entered := make(chan struct{}, 64)
	p := New(ctx, 1, 2, func(ctx context.Context, j Job) (any, error) {
		entered <- struct{}{}
		<-gate
		return j.InputSeq, nil
	})
	// Fill capacity (2 queued) plus the one in-flight job.
	for i := 0; i < 3; i++ {
		if err := p.Submit(ctx, Job{InputSeq: i}); err != nil {
			t.Fatal(err)
		}
	}
	// The next Submit must block (backpressure); prove it by timing out.
	done := make(chan error, 1)
	go func() { done <- p.Submit(ctx, Job{InputSeq: 99}) }()
	select {
	case err := <-done:
		t.Fatalf("submit should block under full queue, returned: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	p.Close()
	p.Shutdown(time.Second)
}
