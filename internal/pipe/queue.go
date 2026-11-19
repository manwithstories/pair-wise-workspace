// Package pipe moves located packets through a fixed-capacity queue with
// producer backpressure, and orchestrates the parse/transform/verify
// stages. Slow consumers, consumer errors and context cancellation all
// release resources and preserve the last acknowledged packet.
package pipe

import (
	"context"
	"sync"
	"time"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// Item is one queued packet with optional payload.
type Item struct {
	Packet     scan.Packet
	Data       []byte
	EnqueuedAt time.Time
}

// Queue is a bounded packet queue. Push blocks while full (backpressure);
// Abort unblocks all waiters and drains pending items.
type Queue struct {
	ch   chan Item
	done chan struct{}

	mu        sync.Mutex
	abortErr  error
	lastAcked *Item
	closed    bool
}

// NewQueue creates a queue with the given fixed capacity.
func NewQueue(capacity int) *Queue {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue{ch: make(chan Item, capacity), done: make(chan struct{})}
}

// Push enqueues an item, blocking while the queue is full. It returns the
// abort error or ctx error if the queue is torn down first.
func (q *Queue) Push(ctx context.Context, it Item) error {
	it.EnqueuedAt = time.Now()
	select {
	case q.ch <- it:
		return nil
	case <-q.done:
		return q.abortReason()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pop dequeues the next item. ok is false once the queue is closed and
// drained, or aborted.
func (q *Queue) Pop(ctx context.Context) (it Item, ok bool) {
	select {
	case it, ok = <-q.ch:
		return it, ok
	case <-q.done:
		// Drain anything still buffered after abort.
		select {
		case it, ok = <-q.ch:
			return it, ok
		default:
			return Item{}, false
		}
	case <-ctx.Done():
		return Item{}, false
	}
}

// Ack records the last confirmed packet for audit on abnormal shutdown.
func (q *Queue) Ack(it Item) {
	q.mu.Lock()
	c := it
	q.lastAcked = &c
	q.mu.Unlock()
}

// LastAcked returns the most recently acknowledged item.
func (q *Queue) LastAcked() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lastAcked == nil {
		return Item{}, false
	}
	return *q.lastAcked, true
}

// Close signals no more items will be pushed; consumers drain the buffer.
func (q *Queue) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
	q.mu.Unlock()
}

// Abort tears the queue down with err, unblocking producers and consumers.
func (q *Queue) Abort(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.abortErr == nil {
		q.abortErr = err
	}
	select {
	case <-q.done:
	default:
		close(q.done)
	}
}

func (q *Queue) abortReason() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.abortErr != nil {
		return q.abortErr
	}
	return fault.New(fault.KindCanceled, fault.StageQueue, 0, 0, -1, false, "queue aborted")
}

// AbortErr returns the error the queue was aborted with, if any.
func (q *Queue) AbortErr() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.abortErr
}
