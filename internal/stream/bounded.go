// Package stream provides a byte-bounded blocking queue that connects
// the EBML parser (producer) to the timeline/manifest stages (consumer).
// Memory use is capped by a configurable byte budget: when the queue is
// full the producer blocks instead of allocating, so back-pressure
// propagates back to the parser. A slow or cancelled consumer never
// causes silent block loss — enqueue failures are explicit errors.
package stream

import (
	"context"
	"sync"

	"mediapack/internal/clock"
)

// Item is one queued payload plus bookkeeping. Size is the charged
// memory (len(Payload) plus a fixed per-item accounting cost).
type Item struct {
	Track     uint64
	TimeNS    int64
	ByteOff   int64
	Payload   []byte
	Committed bool
}

// PerItemOverhead approximates the slice header and struct fields so a
// flood of tiny blocks cannot bypass the byte budget.
const PerItemOverhead = 64

// BoundedQueue is a context-aware bounded FIFO with peak accounting.
type BoundedQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	items    []Item
	bytes    int64
	limit    int64
	peak     int64
	enqueued int64
	dropped  int64
	closed   bool
}

// NewBoundedQueue creates a queue limited to limitBytes of charged data.
func NewBoundedQueue(limitBytes int64) *BoundedQueue {
	q := &BoundedQueue{limit: limitBytes}
	if limitBytes <= 0 {
		q.limit = 1 << 20
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func itemSize(it Item) int64 {
	return int64(len(it.Payload)) + PerItemOverhead
}

// Enqueue blocks while the queue lacks room for it, honouring ctx. On
// success the item is fully retained; cancellation returns a
// CatCancelled error and the item is never partially queued.
func (q *BoundedQueue) Enqueue(ctx context.Context, it Item) error {
	need := itemSize(it)
	if need > q.limit {
		return clock.NewError(clock.CatOversized,
			"item needs %d bytes, queue limit is %d", need, q.limit)
	}
	// Wake the condition when the context is cancelled so a blocked
	// enqueue cannot outlive its request.
	stop := q.watchContext(ctx)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for q.bytes+need > q.limit {
		if q.closed {
			return clock.NewError(clock.CatCancelled, "enqueue on closed queue")
		}
		if err := ctx.Err(); err != nil {
			return clock.NewError(clock.CatCancelled, "enqueue blocked: %v", err)
		}
		q.cond.Wait()
	}
	if q.closed {
		return clock.NewError(clock.CatCancelled, "enqueue on closed queue")
	}
	q.items = append(q.items, it)
	q.bytes += need
	q.enqueued++
	if q.bytes > q.peak {
		q.peak = q.bytes
	}
	q.cond.Broadcast() // wake consumers
	return nil
}

// TryEnqueue is non-blocking: it returns false (without queuing) when
// the budget is currently exhausted.
func (q *BoundedQueue) TryEnqueue(it Item) (bool, error) {
	need := itemSize(it)
	if need > q.limit {
		return false, clock.NewError(clock.CatOversized,
			"item needs %d bytes, queue limit is %d", need, q.limit)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false, clock.NewError(clock.CatCancelled, "enqueue on closed queue")
	}
	if q.bytes+need > q.limit {
		return false, nil
	}
	q.items = append(q.items, it)
	q.bytes += need
	q.enqueued++
	if q.bytes > q.peak {
		q.peak = q.bytes
	}
	q.cond.Broadcast()
	return true, nil
}

// Dequeue blocks until an item is available, the queue is closed and
// drained, or ctx is cancelled.
func (q *BoundedQueue) Dequeue(ctx context.Context) (Item, error) {
	stop := q.watchContext(ctx)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		if q.closed {
			return Item{}, clock.NewError(clock.CatCancelled, "queue closed and drained")
		}
		if err := ctx.Err(); err != nil {
			return Item{}, clock.NewError(clock.CatCancelled, "dequeue cancelled: %v", err)
		}
		q.cond.Wait()
	}
	it := q.items[0]
	q.items = q.items[1:]
	q.bytes -= itemSize(it)
	q.cond.Broadcast() // wake producers waiting for room
	return it, nil
}

// watchContext broadcasts the condition whenever ctx finishes, ensuring
// blocked Enqueue/Dequeue calls re-check their predicates promptly.
func (q *BoundedQueue) watchContext(ctx context.Context) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.cond.Broadcast()
			q.mu.Unlock()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// TryDequeue returns the next item without blocking; ok is false when
// the queue is currently empty (regardless of closed state).
func (q *BoundedQueue) TryDequeue() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return Item{}, false
	}
	it := q.items[0]
	q.items = q.items[1:]
	q.bytes -= itemSize(it)
	q.cond.Broadcast()
	return it, true
}

// Close marks the queue closed. Already queued items remain dequeuable;
// further enqueues fail. Safe to call multiple times.
func (q *BoundedQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// Stats reports the queue accounting snapshot.
type Stats struct {
	Bytes     int64
	PeakBytes int64
	Length    int
	Limit     int64
	Enqueued  int64
	Dropped   int64
}

// Stats returns a point-in-time accounting snapshot.
func (q *BoundedQueue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{
		Bytes: q.bytes, PeakBytes: q.peak, Length: len(q.items),
		Limit: q.limit, Enqueued: q.enqueued, Dropped: q.dropped,
	}
}

// MarkDropped records an externally rejected item (the queue itself
// never drops, but a cancelled producer may report the would-be count).
func (q *BoundedQueue) MarkDropped(n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropped += n
}
