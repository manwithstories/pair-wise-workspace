// Package pipeline provides the bounded worker pool and the deterministic
// merger used to turn per-segment work into one stable timeline result.
// Context cancellation flows from the entry point through the task queue to
// every worker and the merger.
package pipeline

import (
	"context"
	"fmt"
	"sync"

	"avlab/internal/media"
)

// Job is one unit of work. The implementation must be deterministic: the same
// input must produce the same Result regardless of which worker runs it.
type Job struct {
	Epoch   media.Epoch
	Segment uint32
	Seq     uint16
	Payload []byte
	// Fn performs the actual indexing/hashing work.
	Fn func(ctx context.Context) (any, error)
}

// Result is the ordered output envelope of a completed job.
type Result struct {
	Epoch   media.Epoch
	Segment uint32
	Seq     uint16
	Value   any
	Err     error
}

// PoolStats exposes boundedness counters for verification.
type PoolStats struct {
	Submitted  uint64
	Completed  uint64
	Failed     uint64
	Canceled   uint64
	QueuePeak  int
	MaxInflight int
}

// Pool is a fixed-size worker pool with a bounded task queue.
type Pool struct {
	jobs    chan Job
	results chan Result
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	stats    PoolStats
	inflight int
	closed   bool
}

// NewPool validates limits and starts workers. workers is hard-capped by
// media.MaxWorkers, queue depth by media.MaxQueue.
func NewPool(parent context.Context, workers, queue int) (*Pool, error) {
	if workers <= 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, false,
			"workers must be positive")
	}
	if workers > media.MaxWorkers {
		return nil, media.NewFault(media.CodeLimitExceeded, false,
			fmt.Sprintf("workers %d exceed hard limit %d", workers, media.MaxWorkers))
	}
	if queue <= 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, false,
			"queue depth must be positive")
	}
	if queue > media.MaxQueue {
		return nil, media.NewFault(media.CodeLimitExceeded, false,
			fmt.Sprintf("queue %d exceeds hard limit %d", queue, media.MaxQueue))
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		jobs:    make(chan Job, queue),
		results: make(chan Result, queue),
		ctx:     ctx,
		cancel:  cancel,
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
	return p, nil
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			p.runJob(job)
		}
	}
}

func (p *Pool) runJob(job Job) {
	p.mu.Lock()
	p.inflight++
	if p.inflight > p.stats.MaxInflight {
		p.stats.MaxInflight = p.inflight
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inflight--
		p.mu.Unlock()
	}()
	val, err := job.Fn(p.ctx)
	res := Result{
		Epoch: job.Epoch, Segment: job.Segment, Seq: job.Seq,
		Value: val, Err: err,
	}
	p.mu.Lock()
	switch {
	case p.ctx.Err() != nil || media.IsCode(err, media.CodeCanceled):
		p.stats.Canceled++
	case err != nil:
		p.stats.Failed++
	default:
		p.stats.Completed++
	}
	p.mu.Unlock()
	select {
	case p.results <- res:
	case <-p.ctx.Done():
		// drain on cancellation: keep the count, drop the envelope so the
		// worker can exit even if nobody is reading the result channel
	}
}

// Submit enqueues a job, honoring context cancellation and queue capacity.
func (p *Pool) Submit(job Job) error {
	if job.Fn == nil {
		return media.NewFault(media.CodeInvalidArgument, false, "job without Fn")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return media.NewFault(media.CodeCanceled, false, "pool already closed")
	}
	p.stats.Submitted++
	queued := len(p.jobs) + 1
	if queued > p.stats.QueuePeak {
		p.stats.QueuePeak = queued
	}
	p.mu.Unlock()
	select {
	case <-p.ctx.Done():
		return media.WrapFault(media.CodeCanceled, true,
			"submit interrupted by cancellation", p.ctx.Err())
	case p.jobs <- job:
		return nil
	}
}

// Close stops dispatching, closes the queue and waits for every worker to
// exit. Buffered results remain drainable via Results.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.jobs)
	p.wg.Wait()
	close(p.results)
}

// Cancel requests an orderly shutdown and waits for workers to drain.
func (p *Pool) Cancel() {
	p.cancel()
	p.Close()
}

// Results exposes the raw completion channel for the merger.
func (p *Pool) Results() <-chan Result { return p.results }

// Stats returns a snapshot of pool counters.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}
