// Package pipeline provides the bounded concurrency, stable merge and
// cancellation primitives that let medialine process fragments without
// growing memory with file size or filesystem latency.
package pipeline

import (
	"context"
	"errors"
	"sync"

	"medialine/internal/fault"
)

// ErrClosed is returned when submitting to a closed or cancelled pool.
var ErrClosed = errors.New("pipeline closed")

// Pool is a bounded worker pool. The submission channel has a fixed capacity,
// so a slow consumer pushes back on the producer rather than letting an
// unbounded queue grow. The number of in-flight items is bounded by
// queue+workers and is observable for the demo metrics.
type Pool struct {
	workers  int
	queue    int
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     chan job
	wg       sync.WaitGroup
	closer   sync.Once
	mu       sync.Mutex
	inflight int
	peak     int
	accepted int
	failed   int
	// errFn maps a job panic or error into a fault; set by the caller.
	errFn func(interface{}) *fault.Error
	// logFn receives recoverable faults.
	logFn func(*fault.Error)
}

type job struct {
	seq  int64
	run  func(context.Context) error
	done func(int64, error)
}

// Config sizes the pool.
type Config struct {
	// Workers is the number of concurrent workers; must be >= 1.
	Workers int
	// Queue is the submission channel capacity; must be >= 0.
	Queue int
	// OnFault observes recoverable worker faults.
	OnFault func(*fault.Error)
}

// New creates a pool and starts its workers.
func New(parent context.Context, cfg Config) (*Pool, error) {
	if cfg.Workers < 1 {
		return nil, fault.New(fault.StageConfig, fault.KindBadConfig,
			"worker count %d must be at least 1", cfg.Workers)
	}
	if cfg.Queue < 0 {
		return nil, fault.New(fault.StageConfig, fault.KindBadConfig,
			"queue capacity %d must not be negative", cfg.Queue)
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		workers: cfg.Workers,
		queue:   cfg.Queue,
		ctx:     ctx,
		cancel:  cancel,
		jobs:    make(chan job, cfg.Queue),
		logFn:   cfg.OnFault,
	}
	p.wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go p.worker()
	}
	return p, nil
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			// Drain anything already accepted so callers' done callbacks run.
			p.drain()
			return
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			p.exec(j)
		}
	}
}

// drain processes queued jobs after cancellation, failing them with the
// cancellation fault so no goroutine leaks and every caller is notified.
func (p *Pool) drain() {
	for {
		select {
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			p.release()
			if j.done != nil {
				j.done(j.seq, fault.New(fault.StageCancel, fault.KindCanceled, "pool cancelled before job ran"))
			}
		default:
			return
		}
	}
}

func (p *Pool) exec(j job) {
	defer p.release()
	defer func() {
		if r := recover(); r != nil {
			p.mu.Lock()
			p.failed++
			p.mu.Unlock()
			err := fault.New(fault.StageCancel, fault.KindCorruptSample, "worker panic: %v", r)
			p.report(err)
			if j.done != nil {
				j.done(j.seq, err)
			}
		}
	}()
	var err error
	if p.ctx.Err() != nil {
		err = fault.New(fault.StageCancel, fault.KindCanceled, "pool cancelled")
	} else {
		err = j.run(p.ctx)
	}
	if err != nil {
		p.mu.Lock()
		p.failed++
		p.mu.Unlock()
		if fe, ok := fault.As(err); ok {
			p.report(fe)
		}
	}
	if j.done != nil {
		j.done(j.seq, err)
	}
}

func (p *Pool) report(e *fault.Error) {
	if e != nil && p.logFn != nil {
		p.logFn(e)
	}
}

func (p *Pool) release() {
	p.mu.Lock()
	p.inflight--
	p.mu.Unlock()
}

// Submit queues a job, blocking when the queue is full (backpressure). It
// returns ErrClosed when the pool is cancelled or closed.
func (p *Pool) Submit(run func(context.Context) error, done func(int64, error)) (int64, error) {
	select {
	case <-p.ctx.Done():
		return 0, fault.New(fault.StageCancel, fault.KindCanceled, "pool cancelled")
	default:
	}
	p.mu.Lock()
	p.accepted++
	seq := int64(p.accepted)
	p.mu.Unlock()
	j := job{seq: seq, run: run, done: done}
	select {
	case p.jobs <- j:
		// Count in-flight only after the job is actually accepted by the
		// bounded channel, so a blocked producer cannot inflate the metric.
		p.mu.Lock()
		p.inflight++
		if p.inflight > p.peak {
			p.peak = p.inflight
		}
		p.mu.Unlock()
		return seq, nil
	case <-p.ctx.Done():
		return seq, fault.New(fault.StageCancel, fault.KindCanceled, "pool cancelled while submitting")
	}
}

// Cancel stops accepting work and signals workers to drain. It is idempotent.
func (p *Pool) Cancel() { p.cancel() }

// Close stops the pool and waits for every worker to exit. It must be called
// exactly once after the producer has finished submitting.
func (p *Pool) Close() {
	p.closer.Do(func() {
		p.cancel()
		close(p.jobs)
	})
	p.wg.Wait()
}

// Context exposes the pool's cancellation context for producers.
func (p *Pool) Context() context.Context { return p.ctx }

// Stats snapshots pool counters.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Workers:  p.workers,
		Queue:    p.queue,
		Accepted: p.accepted,
		Failed:   p.failed,
		Inflight: p.inflight,
		Peak:     p.peak,
	}
}

// Stats is a point-in-time pool snapshot. Peak is the largest number of
// accepted-but-unfinished jobs, which is bounded by Queue+Workers.
type Stats struct {
	Workers  int
	Queue    int
	Accepted int
	Failed   int
	Inflight int
	Peak     int
}

// Limit reports the structural bound on in-flight work.
func (s Stats) Limit() int { return s.Workers + s.Queue }
