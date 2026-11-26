// Package stream runs the bounded multi-stage pipeline: ingest -> repair ->
// sink/index, with byte-accounted back pressure and hash-based checkpoints
// that make interruption and resumption deterministic.
package stream

import (
	"context"
	"sync"

	"relay/internal/media"
)

// Item is one unit of work moving through the pipeline. Weight is its byte
// cost against the queue-bytes budget (raw fragment bytes and buffered
// output both count).
type Item struct {
	Key    string // stable per-fragment key, e.g. "track/sequence"
	Weight int64
	Value  interface{}
}

// BoundedQueue is a FIFO channel-like queue whose total queued weight never
// exceeds a configured byte budget. A slow consumer blocks producers inside
// Enqueue (back pressure); the implementation never falls back to an
// unbounded buffer.
type BoundedQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []Item
	queued  int64
	limit   int64
	peak    int64
	closed  bool
}

// NewBoundedQueue creates a queue with the given queue-bytes ceiling.
func NewBoundedQueue(limit int64) *BoundedQueue {
	q := &BoundedQueue{limit: limit}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue blocks until the item's weight fits within the remaining budget or
// ctx ends. It returns media.FaultQueueLimit only when the item alone exceeds
// the configured ceiling, which is a configuration/input error.
func (q *BoundedQueue) Enqueue(ctx context.Context, it Item) error {
	if it.Weight > q.limit {
		return media.NewFault(media.FaultQueueLimit, 0, 0, 0,
			"single item exceeds queue-bytes budget")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.queued+it.Weight > q.limit && !q.closed {
		// Wake on cancellation without racing the condition broadcast.
		done := make(chan struct{})
				go func() {
			select {
			case <-ctx.Done():
				q.cond.Broadcast()
			case <-done:
			}
		}()
		q.cond.Wait()
		close(done)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if q.closed {
		return context.Canceled
	}
	q.items = append(q.items, it)
	q.queued += it.Weight
	if q.queued > q.peak {
		q.peak = q.queued
	}
	q.cond.Broadcast()
	return nil
}

// Dequeue blocks until an item is available. The second result is false when
// the queue is closed and drained, mirroring a closed channel.
func (q *BoundedQueue) Dequeue(ctx context.Context) (Item, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				q.cond.Broadcast()
			case <-done:
			}
		}()
		q.cond.Wait()
		close(done)
		if ctx.Err() != nil {
			return Item{}, false, ctx.Err()
		}
	}
	if len(q.items) == 0 {
		return Item{}, false, nil
	}
	it := q.items[0]
	q.items = q.items[1:]
	q.queued -= it.Weight
	q.cond.Broadcast()
	return it, true, nil
}

// Close marks the queue closed; pending items remain drainable.
func (q *BoundedQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Peak returns the high-water mark of queued bytes observed so far.
func (q *BoundedQueue) Peak() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// Queued returns the currently buffered byte weight.
func (q *BoundedQueue) Queued() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queued
}
