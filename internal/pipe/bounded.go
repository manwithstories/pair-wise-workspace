package pipe

import (
	"context"
	"sync/atomic"
)

// Queue is a bounded FIFO with cancellation-aware send and receive. Capacity
// is fixed at construction, so producer memory cannot grow with the archive.
type Queue[T any] struct {
	ch     chan T
	peak   atomic.Int64
	closed atomic.Bool
}

// NewQueue allocates a queue with the given capacity. Capacity must be > 0;
// callers validate the CLI flag before constructing.
func NewQueue[T any](capacity int) *Queue[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue[T]{ch: make(chan T, capacity)}
}

// Cap returns the configured capacity.
func (q *Queue[T]) Cap() int { return cap(q.ch) }

// Len returns the current occupancy.
func (q *Queue[T]) Len() int { return len(q.ch) }

// Send blocks until the value is queued or the context is cancelled. It never
// grows the channel.
func (q *Queue[T]) Send(ctx context.Context, v T) error {
	if q.closed.Load() {
		return ErrClosed
	}
	select {
	case q.ch <- v:
		if n := int64(len(q.ch)); n > q.peak.Load() {
			q.peak.Store(n)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TrySend queues without blocking; it reports whether the value was accepted.
func (q *Queue[T]) TrySend(v T) bool {
	if q.closed.Load() {
		return false
	}
	select {
	case q.ch <- v:
		if n := int64(len(q.ch)); n > q.peak.Load() {
			q.peak.Store(n)
		}
		return true
	default:
		return false
	}
}

// Receive blocks for the next value or returns the context error.
func (q *Queue[T]) Receive(ctx context.Context) (T, error) {
	var zero T
	select {
	case v, ok := <-q.ch:
		if !ok {
			return zero, ErrClosed
		}
		return v, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Close prevents further sends. Receivers drain remaining values.
func (q *Queue[T]) Close() {
	if q.closed.Swap(true) {
		return
	}
	close(q.ch)
}

// Peak returns the highest observed occupancy.
func (q *Queue[T]) Peak() int64 { return q.peak.Load() }
