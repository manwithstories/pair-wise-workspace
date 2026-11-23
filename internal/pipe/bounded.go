package pipe

import (
	"context"
	"sync"
	"sync/atomic"
)

// Queue is a bounded FIFO with cancellation-aware send and receive. Capacity
// is fixed at construction, so producer memory cannot grow with the archive.
type Queue[T any] struct {
	ch        chan T
	peak      atomic.Int64
	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closeCh   chan struct{}
}

// NewQueue allocates a queue with the given capacity. Capacity must be > 0;
// callers validate the CLI flag before constructing.
func NewQueue[T any](capacity int) *Queue[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue[T]{ch: make(chan T, capacity), closeCh: make(chan struct{})}
}

// Cap returns the configured capacity.
func (q *Queue[T]) Cap() int { return cap(q.ch) }

// Len returns the current occupancy.
func (q *Queue[T]) Len() int { return len(q.ch) }

// Send blocks until the value is queued, the context is cancelled, or the
// queue is closed. It never grows the channel.
func (q *Queue[T]) Send(ctx context.Context, v T) error {
	select {
	case <-q.closeCh:
		return ErrClosed
	default:
	}
	select {
	case q.ch <- v:
		q.notePeak()
		return nil
	case <-q.closeCh:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TrySend queues without blocking; it reports whether the value was accepted.
func (q *Queue[T]) TrySend(v T) bool {
	select {
	case <-q.closeCh:
		return false
	default:
	}
	select {
	case q.ch <- v:
		q.notePeak()
		return true
	default:
		return false
	}
}

func (q *Queue[T]) notePeak() {
	if n := int64(len(q.ch)); n > q.peak.Load() {
		q.peak.Store(n)
	}
}

// Receive blocks for the next value or returns the context error. Once the
// queue is closed and drained it returns ErrClosed; it does not distinguish
// that from an empty-but-closed queue, matching stage shutdown semantics.
func (q *Queue[T]) Receive(ctx context.Context) (T, error) {
	var zero T
	select {
	case v := <-q.ch:
		return v, nil
	default:
	}
	select {
	case v := <-q.ch:
		return v, nil
	case <-q.closeCh:
		// Drain a value that arrived concurrently with Close.
		select {
		case v := <-q.ch:
			return v, nil
		default:
			return zero, ErrClosed
		}
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Close prevents further sends. Receivers drain remaining values and then see
// ErrClosed. The data channel is left open so a send racing with Close can
// never panic; senders instead observe closeCh.
func (q *Queue[T]) Close() {
	q.closeOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		close(q.closeCh)
		q.mu.Unlock()
	})
}

// Peak returns the highest observed occupancy.
func (q *Queue[T]) Peak() int64 { return q.peak.Load() }
