// Package stream wires the bounded queue and the pipeline runner. The queue
// backs the producer/consumer hand-off; capacity is fixed at construction and
// a slow consumer blocks the producer instead of growing memory. Cancellation
// unblocks every waiter while preserving the last complete block.
package stream

import (
	"context"
	"fmt"
	"sync"

	"avpipe/internal/media"
)

// Queue is a bounded multi-producer, multi-consumer queue.
type Queue[T any] struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	items    []T
	capacity int
	closed   bool
	// stats
	highWater      int
	enqueued       uint64
	dequeued       uint64
	backpressureWA uint64
}

// NewQueue creates a queue with the given capacity. Capacity must be positive;
// the queue never grows beyond it.
func NewQueue[T any](capacity int) (*Queue[T], error) {
	if capacity <= 0 {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "queue",
			fmt.Errorf("queue capacity must be positive, got %d", capacity))
	}
	q := &Queue[T]{items: make([]T, 0, capacity), capacity: capacity}
	q.notEmpty = sync.NewCond(&q.mu)
	q.notFull = sync.NewCond(&q.mu)
	return q, nil
}

// Push enqueues v, blocking while the queue is full. It returns ctx.Err() if
// the context is cancelled or ErrClosed if the queue was closed. On failure
// nothing is enqueued.
func (q *Queue[T]) Push(ctx context.Context, v T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == q.capacity && !q.closed {
		if err := ctx.Err(); err != nil {
			return err
		}
		q.backpressureWA++
		// A context-aware wait: the stop goroutine broadcasts on both conds,
		// so a cancelled context wakes this loop at the next check.
		stop := q.watch(ctx)
		q.notFull.Wait()
		stop()
	}
	if q.closed {
		return media.NewError(media.PhaseQueue, media.ExitInternal, "", 0, false, "push",
			ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	q.items = append(q.items, v)
	q.enqueued++
	if len(q.items) > q.highWater {
		q.highWater = len(q.items)
	}
	q.notEmpty.Signal()
	return nil
}

// Pop removes the oldest item, blocking while the queue is empty. It returns
// io.EOF-like ErrClosed once the queue is drained and closed.
func (q *Queue[T]) Pop(ctx context.Context) (T, error) {
	var zero T
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		if q.closed {
			return zero, media.NewError(media.PhaseQueue, media.ExitInternal, "", 0, false, "pop", ErrClosed)
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		stop := q.watch(ctx)
		q.notEmpty.Wait()
		stop()
	}
	v := q.items[0]
	q.items = q.items[1:]
	q.dequeued++
	q.compact()
	q.notFull.Signal()
	return v, nil
}

// TryPop removes the oldest item without blocking.
func (q *Queue[T]) TryPop() (T, bool) {
	var zero T
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return zero, false
	}
	v := q.items[0]
	q.items = q.items[1:]
	q.dequeued++
	q.compact()
	q.notFull.Signal()
	return v, true
}

// compact restores the backing array when slicing has reduced its tail
// capacity, keeping the usable window equal to the configured capacity.
func (q *Queue[T]) compact() {
	if q.capacity <= 1 || cap(q.items) >= q.capacity {
		return
	}
	compact := make([]T, len(q.items), q.capacity)
	copy(compact, q.items)
	q.items = compact
}

// Close marks the queue closed after its current items are drained, waking all
// waiters. It is idempotent.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
}

// Closed reports whether Close has been called.
func (q *Queue[T]) Closed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// Len returns the current occupancy.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Cap returns the fixed capacity.
func (q *Queue[T]) Cap() int { return q.capacity }

// Stats is a snapshot of queue counters.
type Stats struct {
	Capacity     int
	Len          int
	HighWater    int
	Enqueued     uint64
	Dequeued     uint64
	Backpressure uint64
}

// Stats snapshots the counters.
func (q *Queue[T]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{
		Capacity:     q.capacity,
		Len:          len(q.items),
		HighWater:    q.highWater,
		Enqueued:     q.enqueued,
		Dequeued:     q.dequeued,
		Backpressure: q.backpressureWA,
	}
}

// ErrClosed is returned when a closed queue cannot satisfy an operation.
var ErrClosed = fmt.Errorf("queue closed")

// watch starts a goroutine that broadcasts when ctx is done, waking any
// cond.Wait. The returned stop function must be called after the wait to
// release the watcher.
func (q *Queue[T]) watch(ctx context.Context) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.notEmpty.Broadcast()
			q.notFull.Broadcast()
		case <-done:
		}
	}()
	return func() { close(done) }
}
