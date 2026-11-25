// Package pipeline runs frame work through a bounded worker pool and merges
// results deterministically. Cancellation propagates from the producer through
// workers to the writer; workers stop claiming work, the queue is closed, and
// no partial output is left behind.
package pipeline

import (
	"context"
	"sync"

	"relay.local/relay/internal/fault"
)

// Pool is a bounded worker pool. Capacity bounds both concurrency and the
// number of queued items, so a slow consumer applies backpressure to the
// producer instead of growing memory.
type Pool struct {
	workers  int
	inflight int
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     chan Job
	done     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	canceled bool
	closed   bool
}

// Job is one unit of work. ID is used only for diagnostics and ordering.
type Job struct {
	ID   int
	Run  func(ctx context.Context) (any, error)
	Sink func(any)
	// Err receives a per-job error without stopping the pool.
	Err func(error)
}

// Config bounds the pool.
type Config struct {
	// Workers is the number of concurrent goroutines; must be positive.
	Workers int
	// Inflight is the queue capacity between producer and workers.
	Inflight int
}

// New creates a pool and starts its workers.
func New(ctx context.Context, cfg Config) (*Pool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Workers <= 0 {
		return nil, fault.New(-1, 0, fault.KindLimit, false, "pipeline: workers must be positive")
	}
	if cfg.Inflight < 0 {
		return nil, fault.New(-1, 0, fault.KindLimit, false, "pipeline: inflight must be non-negative")
	}
	if cfg.Inflight == 0 {
		cfg.Inflight = cfg.Workers
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &Pool{
		workers:  cfg.Workers,
		inflight: cfg.Inflight,
		ctx:      pctx,
		cancel:   cancel,
		jobs:     make(chan Job, cfg.Inflight),
		done:     make(chan struct{}),
	}
	p.wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go p.worker()
	}
	return p, nil
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		if p.ctx.Err() != nil {
			// Drain without running so the producer's send never blocks after
			// cancellation.
			continue
		}
		val, err := p.runJob(job)
		if err != nil {
			if job.Err != nil {
				job.Err(err)
			}
			continue
		}
		if job.Sink != nil && p.ctx.Err() == nil {
			job.Sink(val)
		}
	}
}

func (p *Pool) runJob(job Job) (val any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fault.New(-1, 0, fault.KindCorrupt, true, "pipeline: job panicked: %v", r)
		}
	}()
	return job.Run(p.ctx)
}

// Submit queues a job, returning false when the pool has been cancelled, is
// closed, or the context is done. Submit never blocks forever: it selects on
// the context.
func (p *Pool) Submit(job Job) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()
	select {
	case <-p.ctx.Done():
		return false
	case <-p.done:
		return false
	case p.jobs <- job:
		return true
	}
}

// Cancel stops accepting work and signals every worker.
func (p *Pool) Cancel() {
	p.mu.Lock()
	p.canceled = true
	p.mu.Unlock()
	p.cancel()
}

// wasCanceled reports whether the caller explicitly cancelled the pool.
func (p *Pool) wasCanceled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.canceled
}

// Canceled reports whether the pool was cancelled by the caller or by its
// parent context.
func (p *Pool) Canceled() bool {
	p.mu.Lock()
	c := p.canceled
	p.mu.Unlock()
	return c || p.ctx.Err() != nil
}

// Close stops the pool: it closes the job queue so workers finish their
// current item and exit, then waits for them. The caller must ensure the
// producer has stopped submitting before calling Close. Close is idempotent.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.done)
	close(p.jobs)
	p.wg.Wait()
	// Release the context's resources only after workers are gone, so Close
	// alone never turns a completed run into a cancelled one.
	p.cancel()
}

// Workers returns the configured concurrency.
func (p *Pool) Workers() int { return p.workers }

// Inflight returns the configured queue capacity.
func (p *Pool) Inflight() int { return p.inflight }
