package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

func job(order uint64, track uint32, seq uint32, size int) Job {
	ns := clock.NormalizedSample{
		Sample: demux.Sample{
			TrackID: track, Seq: seq, Size: uint32(size),
			Payload: make([]byte, size),
		},
	}
	return Job{Order: order, Sample: ns}
}

func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{Workers: 0, Queue: 1, MaxBufferedBytes: 1},
		{Workers: 1, Queue: 0, MaxBufferedBytes: 1},
		{Workers: 1, Queue: 1, MaxBufferedBytes: 0},
	}
	for _, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Fatalf("invalid config accepted: %+v", cfg)
		}
	}
}

func TestHashAndOrderedResults(t *testing.T) {
	p, err := New(Config{Workers: 4, Queue: 4, MaxBufferedBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	jobs := make(chan Job, 4)
	results := p.Run(ctx, jobs)
	const n = 50
	// Producer and consumer run concurrently: each Acquire is balanced by the
	// worker's Release, so the memory semaphore never fills into a deadlock.
	go func() {
		for i := uint64(1); i <= n; i++ {
			if err := p.Acquire(ctx, 16); err != nil {
				return
			}
			jobs <- job(i, 1, uint32(i), 16)
		}
		close(jobs)
	}()
	seen := map[uint64]bool{}
	for r := range results {
		if r.Err != nil {
			t.Fatalf("validation error: %v", r.Err)
		}
		var zero [32]byte
		if r.Sample.SHA256 == zero {
			t.Fatal("worker did not compute payload hash")
		}
		if seen[r.Order] {
			t.Fatalf("duplicate order %d", r.Order)
		}
		seen[r.Order] = true
	}
	if len(seen) != n {
		t.Fatalf("results = %d, want %d", len(seen), n)
	}
}

func TestCancellationPropagates(t *testing.T) {
	p, _ := New(Config{Workers: 2, Queue: 2, MaxBufferedBytes: 64})
	ctx, cancel := context.WithCancel(context.Background())
	// Occupy part of the budget, then cancel before a blocking Acquire: the
	// producer must return promptly instead of hanging.
	if err := p.Acquire(ctx, 32); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- p.Acquire(ctx, 16) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach blocked producer")
	}

	// A second pool with a fresh context processes one job and closes its
	// results channel cleanly, proving worker goroutines do not leak.
	p2, _ := New(Config{Workers: 2, Queue: 2, MaxBufferedBytes: 64})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	jobs := make(chan Job)
	results := p2.Run(ctx2, jobs)
	go func() {
		_ = p2.Acquire(ctx2, 16)
		jobs <- job(1, 1, 1, 16)
		close(jobs)
	}()
	deadline := time.After(time.Second)
	got := 0
	for {
		select {
		case _, ok := <-results:
			if !ok {
				if got == 0 {
					t.Fatal("no result delivered before close")
				}
				return
			}
			got++
		case <-deadline:
			t.Fatal("results channel not closed after jobs drained")
		}
	}
}

func TestOversizedSampleRejected(t *testing.T) {
	p, _ := New(Config{Workers: 1, Queue: 1, MaxBufferedBytes: 32})
	err := p.Acquire(context.Background(), 64)
	var me *model.Error
	if !errors.As(err, &me) {
		t.Fatalf("oversized sample must yield model.Error, got %v", err)
	}
}

func TestWorkerCountIndependence(t *testing.T) {
	// The same job set through different worker counts must produce the same
	// set of hashes; ordering is restored downstream, here we compare the
	// unordered hash multiset.
	run := func(workers int) map[[32]byte]int {
		p, _ := New(Config{Workers: workers, Queue: 8, MaxBufferedBytes: 1 << 20})
		ctx := context.Background()
		jobs := make(chan Job, 8)
		results := p.Run(ctx, jobs)
		go func() {
			for i := uint64(1); i <= 40; i++ {
				_ = p.Acquire(ctx, 8)
				jobs <- job(i, 1, uint32(i), 8)
			}
			close(jobs)
		}()
		out := map[[32]byte]int{}
		for r := range results {
			out[r.Sample.SHA256]++
		}
		return out
	}
	a, b := run(1), run(8)
	if len(a) != len(b) {
		t.Fatalf("hash sets differ by worker count: %d vs %d", len(a), len(b))
	}
	for h, n := range a {
		if b[h] != n {
			t.Fatalf("hash count mismatch for %x", h)
		}
	}
}
