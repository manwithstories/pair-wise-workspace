// Package pool implements a bounded worker pool with backpressure and
// context-driven cancellation. Producers block on a full queue instead of
// buffering unbounded work; cancellation propagates through producer, workers
// and consumer, and Shutdown waits a bounded time for every goroutine to exit.
package pool

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrPoolClosed is returned when work is submitted after the pool stops.
var ErrPoolClosed = errors.New("worker pool closed")

// Job is one unit of work keyed by an input sequence number used for
// deterministic downstream ordering.
type Job struct {
	InputSeq int
	TrackID  uint32
	Payload  any
}

// Result is the processed output paired with its originating job metadata.
type Result struct {
	InputSeq int
	TrackID  uint32
	Out      any
	Err      error
}

// ProcessFunc transforms a job; it must honor ctx cancellation.
type ProcessFunc func(ctx context.Context, j Job) (any, error)

// Pool runs a fixed number of workers over a capacity-bounded job queue.
type Pool struct {
	jobs    chan Job
	results chan Result
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	proc    ProcessFunc
}

// New starts a pool with the given worker count and queue capacity. A capacity
// of zero is treated as one so backpressure always has a defined bound.
func New(parent context.Context, workers, capacity int, proc ProcessFunc) *Pool {
	if workers < 1 {
		workers = 1
	}
	if capacity < 1 {
		capacity = 1
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		jobs:    make(chan Job, capacity),
		results: make(chan Result, capacity),
		cancel:  cancel,
		proc:    proc,
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker(ctx)
	}
	return p
}

func (p *Pool) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			out, err := p.proc(ctx, j)
			r := Result{InputSeq: j.InputSeq, TrackID: j.TrackID, Out: out, Err: err}
			select {
			case p.results <- r:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Submit enqueues a job, blocking under backpressure or until cancellation.
func (p *Pool) Submit(ctx context.Context, j Job) error {
	// Check cancellation first so a stopped pool never accepts work even when
	// the queue still has room.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case p.jobs <- j:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Results exposes the buffered result channel for the consumer.
func (p *Pool) Results() <-chan Result { return p.results }

// Close stops accepting jobs and drains workers.
func (p *Pool) Close() {
	close(p.jobs)
}

// Cancel signals every goroutine to stop.
func (p *Pool) Cancel() { p.cancel() }

// Shutdown cancels work and waits up to timeout for all workers to exit,
// returning true when they did. It never blocks beyond timeout.
func (p *Pool) Shutdown(timeout time.Duration) bool {
	p.cancel()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
