// Package worker provides the bounded processing stage between demux and
// merge. Packets are normalised concurrently, then re-emitted in exactly the
// order they were submitted, so the merge stage always sees a deterministic
// arrival stream regardless of worker scheduling.
//
// Both the work queue and the reorder buffer are strictly bounded. When a
// bound is reached the producer blocks (backpressure) rather than dropping or
// buffering without limit. Cancellation propagates from the producer through
// every worker to the emitter; the pool always waits for all of its goroutines
// to finish before returning.
package worker

import (
	"context"
	"errors"
	"sync"

	"avpipe/internal/media"
)

// ErrCanceled is returned when processing is canceled.
var ErrCanceled = errors.New("worker: canceled")

// Config bounds the pool.
type Config struct {
	// Workers is the number of concurrent normalisation goroutines.
	Workers int
	// Queue is the capacity of the input queue. Producers block when full.
	Queue int
	// Cache is the capacity of the reorder output buffer. It must be at least
	// Queue so a slow consumer cannot deadlock the pool.
	Cache int
}

// Validate fills and checks defaults.
func (c Config) Validate() (Config, error) {
	if c.Workers <= 0 {
		c.Workers = 1
	}
	if c.Queue <= 0 {
		c.Queue = 8
	}
	if c.Cache <= 0 {
		c.Cache = c.Queue
	}
	if c.Cache < c.Queue {
		return c, media.Errorf(media.KindUsage, media.Origin{Stage: "worker"},
			"worker.Config", "cache size %d must be >= queue size %d", c.Cache, c.Queue)
	}
	if c.Workers > 1024 {
		return c, media.Errorf(media.KindUsage, media.Origin{Stage: "worker"},
			"worker.Config", "worker count %d exceeds limit 1024", c.Workers)
	}
	return c, nil
}

// Func normalises one packet. It must return quickly and must not retain the
// input packet across calls.
type Func func(ctx context.Context, p media.Packet) (media.Packet, error)

// Stats reports observed bounds for the terminal summary.
type Stats struct {
	Processed int64
	QueuePeak int
	CachePeak int
	Workers   int
	Canceled  bool
	Failures  int64
}

type job struct {
	seq int64
	pkt media.Packet
}

type result struct {
	seq int64
	pkt media.Packet
	err error
}

// Pool is a bounded, order-preserving concurrent normaliser.
type Pool struct {
	cfg Config
	fn  Func

	in      chan media.Packet
	jobs    chan job
	results chan result
	out     chan media.Packet

	ctx    context.Context
	cancel context.CancelFunc
	parent context.Context

	workers sync.WaitGroup
	all     sync.WaitGroup

	mu        sync.Mutex
	queuePeak int
	cachePeak int
	processed int64
	failures  int64
	err       error
	closed    bool
	closeOnce sync.Once
	done      chan struct{}

	// subMu guards submissions against a concurrent Close. It is an RWMutex:
	// Submit holds RLock while it may block on the input queue; Close closes
	// done first (so blocked submits abort), then takes Lock, then closes the
	// input channel once no submit is in flight.
	subMu sync.RWMutex
}

// Submit enqueues a packet, blocking when the queue is full. It returns
// ErrCanceled once the pool has stopped accepting work, and is safe to call
// concurrently with Close.
func (p *Pool) Submit(pkt media.Packet) error {
	p.subMu.RLock()
	defer p.subMu.RUnlock()
	select {
	case <-p.done:
		return ErrCanceled
	default:
	}
	select {
	case <-p.done:
		return ErrCanceled
	case <-p.ctx.Done():
		return ErrCanceled
	case p.in <- pkt:
		p.mu.Lock()
		if n := len(p.in); n > p.queuePeak {
			p.queuePeak = n
		}
		p.mu.Unlock()
		return nil
	}
}

// Close stops accepting new packets and drains in-flight work. It is safe to
// call more than once and concurrently with Submit; the output channel is
// closed when everything has been emitted.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		// 1. Signal that no new submissions are accepted. Every blocked or
		//    future Submit wakes on done and returns.
		close(p.done)
		// 2. Wait until every in-flight Submit has released its read lock.
		p.subMu.Lock()
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		// 3. No submit can be touching p.in now, so closing it is safe.
		close(p.in)
		p.subMu.Unlock()
	})
	p.all.Wait()
}

// New starts a pool. Close must be called to drain it; Cancel aborts early.
func New(parent context.Context, cfg Config, fn Func) (*Pool, error) {
	c, err := cfg.Validate()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		cfg: c, fn: fn, parent: parent, ctx: ctx, cancel: cancel,
		in:      make(chan media.Packet, c.Queue),
		jobs:    make(chan job, c.Queue),
		results: make(chan result, c.Workers),
		out:     make(chan media.Packet, c.Cache),
		done:    make(chan struct{}),
	}
	p.all.Add(2)
	go p.dispatch()
	go p.emit()
	p.workers.Add(c.Workers)
	for i := 0; i < c.Workers; i++ {
		go p.run()
	}
	// When every worker exits, close the result stream so the emitter knows
	// no more results can arrive.
	go func() {
		p.workers.Wait()
		close(p.results)
	}()
	return p, nil
}

// Cancel aborts processing. Workers and the dispatcher stop, and the output
// channel closes after buffered results are drained.
func (p *Pool) Cancel() { p.cancel() }

// Results returns the order-preserving output channel.
func (p *Pool) Results() <-chan media.Packet { return p.out }

// Err returns the first fatal error encountered.
func (p *Pool) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Stats returns a snapshot of the observed bounds.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Processed: p.processed,
		QueuePeak: p.queuePeak,
		CachePeak: p.cachePeak,
		Workers:   p.cfg.Workers,
		Canceled:  p.ctx.Err() != nil,
		Failures:  p.failures,
	}
}

func (p *Pool) dispatch() {
	defer p.all.Done()
	defer close(p.jobs)
	var seq int64
	for {
		select {
		case <-p.ctx.Done():
			return
		case pkt, ok := <-p.in:
			if !ok {
				return
			}
			select {
			case <-p.ctx.Done():
				return
			case p.jobs <- job{seq: seq, pkt: pkt}:
				seq++
			}
		}
	}
}

func (p *Pool) run() {
	defer p.workers.Done()
	for j := range p.jobs {
		if p.ctx.Err() != nil {
			return
		}
		out, err := p.fn(p.ctx, j.pkt)
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		res := result{seq: j.seq, pkt: out, err: err}
		if err != nil {
			p.fail(err)
		}
		select {
		case <-p.ctx.Done():
			return
		case p.results <- res:
			if err != nil {
				return
			}
		}
	}
}

// emit reorders results by submission sequence and forwards them in order.
func (p *Pool) emit() {
	defer p.all.Done()
	defer close(p.out)
	pending := make(map[int64]result)
	var next int64
	for res := range p.results {
		if res.err != nil {
			p.fail(res.err)
			return
		}
		pending[res.seq] = res
		for {
			r, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			p.mu.Lock()
			p.processed++
			p.mu.Unlock()
			select {
			case <-p.ctx.Done():
				return
			case p.out <- r.pkt:
				p.mu.Lock()
				if n := len(p.out); n > p.cachePeak {
					p.cachePeak = n
				}
				p.mu.Unlock()
			}
		}
	}
}

func (p *Pool) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
		p.failures++
	}
	p.mu.Unlock()
	p.cancel()
}
