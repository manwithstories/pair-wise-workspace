// Package pipeline provides the bounded byte-weighted stage pipeline used by
// the FieldPost delivery kernel.
package pipeline

import (
	"context"
	"sync"
	"sync/atomic"

	"fieldpost/internal/core"
)

// Item is one bounded unit moving through the pipeline. Weight must include
// all payload bytes retained in memory while the item is queued or handled.
type Item struct {
	ID     string
	Camera string
	Weight int64
	Data   []byte
	Meta   map[string]string
}

// Snapshot is an immutable observability reading.
type Snapshot struct {
	Capacity           int64
	InFlight           int64
	PeakInFlight       int64
	BackpressureEvents int64
	Processed          int64
	Failed             int64
}

// Gate is a byte-weighted memory bound with backpressure accounting.
type Gate struct {
	capacity int64
	mu       sync.Mutex
	current  int64
	peak     int64
	events   int64
	wake     chan struct{}
}

// NewGate creates a gate with capacity bytes.
func NewGate(capacity int64) *Gate {
	if capacity <= 0 {
		capacity = 8 << 20
	}
	return &Gate{capacity: capacity, wake: make(chan struct{})}
}

// Reserve blocks until n bytes fit. It records one backpressure event the
// first time a caller has to wait. Cancellation unblocks waiters.
func (g *Gate) Reserve(ctx context.Context, n int64) error {
	if n < 0 {
		n = 0
	}
	if n > g.capacity {
		return core.InvariantError(core.StagePipeline, "memory-gate", n,
			"item weight %d exceeds memory cap %d", n, g.capacity)
	}
	waited := false
	for {
		g.mu.Lock()
		if g.current+n <= g.capacity {
			g.current += n
			if g.current > g.peak {
				g.peak = g.current
			}
			wake := g.wake
			g.mu.Unlock()
			_ = wake
			return nil
		}
		if !waited {
			g.events++
			waited = true
		}
		wake := g.wake
		g.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return core.Wrap(core.StagePipeline, 1, false, ctx.Err(), "memory reserve canceled")
		}
	}
}

// Release returns n bytes and wakes one or all waiters via broadcast.
func (g *Gate) Release(n int64) {
	g.mu.Lock()
	g.current -= n
	if g.current < 0 {
		g.current = 0
	}
	close(g.wake)
	g.wake = make(chan struct{})
	g.mu.Unlock()
}

// Snapshot returns peak and backpressure counters.
func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{Capacity: g.capacity, InFlight: g.current, PeakInFlight: g.peak,
		BackpressureEvents: g.events}
}

type job struct {
	item Item
}

// Handler processes one pipeline item.
type Handler func(context.Context, Item) error

// Pipeline is a fixed-worker bounded pipeline. Submit applies byte
// backpressure before enqueue; Close drains and reclaims every worker.
type Pipeline struct {
	gate      *Gate
	jobs      chan job
	wg        sync.WaitGroup
	closeOnce sync.Once
	cancel    context.CancelFunc
	ctx       context.Context
	handler   Handler

	errOnce sync.Once
	errMu   sync.RWMutex
	err     error

	processed atomic.Int64
	failed    atomic.Int64
}

// New starts workers. A zero worker count uses one ordered worker.
func New(parent context.Context, capacity int64, workers, queue int, h Handler) *Pipeline {
	if workers <= 0 {
		workers = 1
	}
	if queue <= 0 {
		queue = workers * 4
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pipeline{
		gate: NewGate(capacity), jobs: make(chan job, queue),
		cancel: cancel, ctx: ctx, handler: h,
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *Pipeline) worker() {
	defer p.wg.Done()
	for j := range p.jobs {
		// Even after cancellation, drain queued jobs so Submit/Close cannot
		// strand reserved bytes or goroutines.
		if p.ctx.Err() == nil {
			if err := p.handler(p.ctx, j.item); err != nil {
				p.fail(err)
			} else {
				p.processed.Add(1)
			}
		}
		p.gate.Release(j.item.Weight)
	}
}

func (p *Pipeline) fail(err error) {
	p.errOnce.Do(func() {
		p.errMu.Lock()
		p.err = err
		p.errMu.Unlock()
		p.failed.Add(1)
		p.cancel()
	})
}

func (p *Pipeline) firstErr() error {
	p.errMu.RLock()
	defer p.errMu.RUnlock()
	return p.err
}

// Submit reserves item.Weight and enqueues it. On failure or cancellation no
// bytes remain reserved for this call.
func (p *Pipeline) Submit(ctx context.Context, item Item) error {
	if item.Weight <= 0 {
		item.Weight = int64(len(item.Data))
	}
	if err := p.gate.Reserve(ctx, item.Weight); err != nil {
		return err
	}
	select {
	case p.jobs <- job{item: item}:
		return nil
	case <-p.ctx.Done():
		p.gate.Release(item.Weight)
		if err := p.firstErr(); err != nil {
			return err
		}
		return p.ctx.Err()
	case <-ctx.Done():
		p.gate.Release(item.Weight)
		return ctx.Err()
	}
}

// Close flushes queued items, waits for all workers and returns first failure.
func (p *Pipeline) Close() error {
	p.closeOnce.Do(func() { close(p.jobs) })
	p.wg.Wait()
	p.cancel()
	s := p.gate.Snapshot()
	if s.InFlight != 0 {
		return core.InvariantError(core.StagePipeline, "memory-gate", s.InFlight,
			"pipeline closed with %d bytes still in flight", s.InFlight)
	}
	return p.firstErr()
}

// Snapshot includes gate and completion counters.
func (p *Pipeline) Snapshot() Snapshot {
	s := p.gate.Snapshot()
	s.Processed = p.processed.Load()
	s.Failed = p.failed.Load()
	return s
}

// Err returns the first terminal handler error, if any.
func (p *Pipeline) Err() error { return p.firstErr() }
