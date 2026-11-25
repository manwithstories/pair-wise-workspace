// Package pipeline wires the bounded processing graph: TS demux -> fan-out to
// segmenter / silence detector / fingerprint index -> packager -> manifest.
// Every queue has a hard capacity so a slow consumer applies backpressure all
// the way to the input; cancellation drains in-flight work and preserves the
// last complete-segment checkpoint.
package pipeline

import (
	"context"
	"sync"

	"streamcut/internal/media"
)

// Queue is a capacity-bounded channel wrapper. A full queue blocks the
// sender (backpressure); there is no drop path and no unbounded growth.
type Queue[T any] struct {
	ch     chan T
	peak   int
	mu     sync.Mutex
	closed bool
}

// NewQueue creates a queue holding at most depth items.
func NewQueue[T any](depth int) *Queue[T] {
	return &Queue[T]{ch: make(chan T, depth)}
}

// Send blocks until space is available or ctx is cancelled. On cancellation
// it returns ErrCancelled without touching the queue.
func (q *Queue[T]) Send(ctx context.Context, v T) error {
	// Track the observed fill for buffer_peak_bytes accounting.
	q.mu.Lock()
	if n := len(q.ch); n > q.peak {
		q.peak = n
	}
	q.mu.Unlock()
	select {
	case q.ch <- v:
		return nil
	case <-ctx.Done():
		return media.ErrCancelled
	}
}

// Recv blocks for the next item. ok=false means the queue was closed.
func (q *Queue[T]) Recv(ctx context.Context) (T, bool, error) {
	var zero T
	select {
	case v, ok := <-q.ch:
		return v, ok, nil
	case <-ctx.Done():
		return zero, false, media.ErrCancelled
	}
}

// Close marks the queue drained; repeated Close is safe.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
}

// Len returns the current queued item count.
func (q *Queue[T]) Len() int { return len(q.ch) }

// Peak returns the highest fill observed since creation.
func (q *Queue[T]) Peak() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// Pool is a hard-capped byte buffer pool. Get blocks until a buffer returns
// or ctx cancels; allocations never exceed the configured ceiling.
type Pool struct {
	free   chan []byte
	size   int
	active int
	peak   int
	mu     sync.Mutex
}

// NewPool creates a pool returning buffers of bufSize each, with at most
// maxBuffers outstanding at any time.
func NewPool(bufSize, maxBuffers int) *Pool {
	return &Pool{
		free: make(chan []byte, maxBuffers),
		size: bufSize,
	}
}

// Get obtains a buffer, allocating only while under the ceiling.
func (p *Pool) Get(ctx context.Context) ([]byte, error) {
	select {
	case b := <-p.free:
		return b, nil
	default:
	}
	p.mu.Lock()
	if p.active == cap(p.free) {
		p.mu.Unlock()
		// Ceiling reached: wait for a returned buffer, never over-allocate.
		select {
		case b := <-p.free:
			return b, nil
		case <-ctx.Done():
			return nil, media.ErrCancelled
		}
	}
	p.active++
	if p.active > p.peak {
		p.peak = p.active
	}
	p.mu.Unlock()
	return make([]byte, p.size), nil
}

// Put returns a buffer for reuse.
func (p *Pool) Put(b []byte) {
	if cap(b) != p.size {
		b = make([]byte, p.size)
	}
	select {
	case p.free <- b[:p.size]:
	default: // pool full; let GC reclaim it
	}
}

// PeakBytes reports the largest concurrent buffer footprint.
func (p *Pool) PeakBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(p.peak) * int64(p.size)
}

// MemGuard enforces the global memory ceiling. Stages reserve their working
// set before allocation; a reservation past the limit fails fast with
// ErrResourceLimit rather than growing without bound.
type MemGuard struct {
	limit int64
	used  int64
	peak  int64
	mu    sync.Mutex
}

// NewMemGuard sets the hard process-level memory ceiling in bytes.
func NewMemGuard(limitBytes int64) *MemGuard {
	return &MemGuard{limit: limitBytes}
}

// Reserve books n bytes. It returns ErrResourceLimit past the ceiling.
func (m *MemGuard) Reserve(n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.used+n > m.limit {
		return media.Wrap(media.EResourceLimit, "pipeline.mem",
			"memory ceiling exceeded", nil)
	}
	m.used += n
	if m.used > m.peak {
		m.peak = m.used
	}
	return nil
}

// Release returns a prior reservation.
func (m *MemGuard) Release(n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used -= n
	if m.used < 0 {
		m.used = 0
	}
}

// Peak reports the highest simultaneous reservation.
func (m *MemGuard) Peak() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}
