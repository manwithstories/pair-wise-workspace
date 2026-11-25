// Package pipe connects pipeline stages with byte-bounded queues. Producers
// block when the in-flight byte budget is exhausted (backpressure), so peak
// memory stays independent of input duration. Cancellation and write
// interruptions unblock every stage deterministically.
package pipe

import (
	"context"
	"sync"

	"medpipe/internal/clock"
)

// DefaultInflightBytes is the 8 MiB in-flight budget required by the
// self-test contract.
const DefaultInflightBytes int64 = 8 << 20

// Item is the unit of work handed between stages. Bytes is its accounted
// payload size for the in-flight budget.
type Item struct {
	Stage string
	ID    string
	Bytes int64
	Data  any
}

// Queue is a FIFO byte-bounded queue. Zero value is not usable; construct
// with NewQueue.
type Queue struct {
	mu       sync.Mutex
	notFull  *sync.Cond
	notEmpty *sync.Cond
	items    []*Item
	inflight int64
	peak     int64
	capacity int64
	closed   bool
}

// NewQueue builds a queue with the given in-flight byte capacity.
func NewQueue(capacity int64) (*Queue, error) {
	if capacity <= 0 {
		return nil, clock.E(clock.KindArgument, "pipe.queue",
			clock.WithDetail("capacity must be positive, got %d", capacity))
	}
	q := &Queue{capacity: capacity}
	q.notFull = sync.NewCond(&q.mu)
	q.notEmpty = sync.NewCond(&q.mu)
	return q, nil
}

// Put blocks until item fits within the byte budget or ctx ends. A single
// item larger than the whole capacity is rejected rather than deadlocking.
func (q *Queue) Put(ctx context.Context, item *Item) error {
	if item == nil || item.Bytes < 0 {
		return clock.E(clock.KindArgument, "pipe.queue",
			clock.WithDetail("invalid item: %+v", item))
	}
	if item.Bytes > q.capacity {
		return clock.E(clock.KindArgument, "pipe.queue",
			clock.WithDetail("item %s is %d bytes, over capacity %d",
				item.ID, item.Bytes, q.capacity))
	}
	stop := q.watchCancel(ctx)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed && q.inflight+item.Bytes > q.capacity {
		if err := ctx.Err(); err != nil {
			return clock.E(clock.KindCanceled, "pipe.queue", clock.WithWrap(err))
		}
		q.notFull.Wait()
	}
	if q.closed {
		return clock.E(clock.KindCanceled, "pipe.queue",
			clock.WithDetail("queue closed"))
	}
	if err := ctx.Err(); err != nil {
		return clock.E(clock.KindCanceled, "pipe.queue", clock.WithWrap(err))
	}
	q.items = append(q.items, item)
	q.inflight += item.Bytes
	if q.inflight > q.peak {
		q.peak = q.inflight
	}
	q.notEmpty.Signal()
	return nil
}

// Get blocks until an item is available. It returns (nil, false) after the
// queue is closed and drained, or a canceled error if ctx ends.
func (q *Queue) Get(ctx context.Context) (*Item, bool, error) {
	stop := q.watchCancel(ctx)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		if err := ctx.Err(); err != nil {
			return nil, false, clock.E(clock.KindCanceled, "pipe.queue", clock.WithWrap(err))
		}
		q.notEmpty.Wait()
	}
	if len(q.items) == 0 {
		return nil, false, nil // closed and drained
	}
	item := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	q.inflight -= item.Bytes
	q.notFull.Signal()
	return item, true, nil
}

// Close marks the queue closed; blocked producers fail, consumers drain it.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
	q.mu.Unlock()
}

// PeakInflight reports the highest simultaneous in-flight byte total.
func (q *Queue) PeakInflight() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// Inflight reports the current in-flight byte total.
func (q *Queue) Inflight() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inflight
}

// watchCancel wakes all waiters when ctx ends so Put/Get never hang.
func (q *Queue) watchCancel(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.notEmpty.Broadcast()
			q.notFull.Broadcast()
			q.mu.Unlock()
		case <-done:
		}
	}()
	return func() { close(done) }
}
