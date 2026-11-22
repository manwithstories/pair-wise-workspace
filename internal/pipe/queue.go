// Package pipe moves committed segments through a byte-bounded queue
// with backpressure propagation to the producer.
package pipe

import (
	"errors"
	"sync"
	"time"

	"mediasim/internal/txn"
)

// ErrBackpressureTimeout is returned when a push cannot proceed within
// its timeout because the consumer is too slow.
var ErrBackpressureTimeout = errors.New("pipe: backpressure timeout")

// Item is one queued segment with its accounting size.
type Item struct {
	Segment    txn.Segment
	Size       int
	EnqueuedAt time.Time
}

// Queue is a byte-bounded blocking queue. The used byte count never
// exceeds the configured maximum, so Peak stays within budget.
type Queue struct {
	mu       sync.Mutex
	changed  chan struct{}
	maxBytes int
	used     int
	peak     int
	items    []Item
	closed   bool
}

// NewQueue creates a queue holding at most maxBytes of items.
func NewQueue(maxBytes int) *Queue {
	if maxBytes < 1 {
		maxBytes = 1
	}
	return &Queue{maxBytes: maxBytes, changed: make(chan struct{})}
}

// signalLocked wakes all waiters. Caller holds mu.
func (q *Queue) signalLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// Push enqueues an item, blocking while the byte budget is exhausted.
// It returns ErrBackpressureTimeout if the budget does not free up
// within timeout.
func (q *Queue) Push(it Item, timeout time.Duration) error {
	if it.Size > q.maxBytes {
		return ErrBackpressureTimeout
	}
	deadline := time.Now().Add(timeout)
	q.mu.Lock()
	for !q.closed && q.used+it.Size > q.maxBytes {
		remain := time.Until(deadline)
		if remain <= 0 {
			q.mu.Unlock()
			return ErrBackpressureTimeout
		}
		ch := q.changed
		q.mu.Unlock()
		t := time.NewTimer(remain)
		select {
		case <-ch:
			t.Stop()
		case <-t.C:
			return ErrBackpressureTimeout
		}
		q.mu.Lock()
	}
	if q.closed {
		q.mu.Unlock()
		return errors.New("pipe: queue closed")
	}
	q.items = append(q.items, it)
	q.used += it.Size
	if q.used > q.peak {
		q.peak = q.used
	}
	q.signalLocked()
	q.mu.Unlock()
	return nil
}

// Pop removes the oldest item, blocking until one is available or the
// queue is closed and drained.
func (q *Queue) Pop() (Item, bool) {
	q.mu.Lock()
	for len(q.items) == 0 && !q.closed {
		ch := q.changed
		q.mu.Unlock()
		<-ch
		q.mu.Lock()
	}
	if len(q.items) == 0 {
		q.mu.Unlock()
		return Item{}, false
	}
	it := q.items[0]
	q.items = q.items[1:]
	q.used -= it.Size
	q.signalLocked()
	q.mu.Unlock()
	return it, true
}

// Close marks the queue closed; remaining items stay drainable.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.signalLocked()
	q.mu.Unlock()
}

// Peak returns the maximum used byte count observed.
func (q *Queue) Peak() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}
