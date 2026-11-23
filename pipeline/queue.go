// Package pipeline wires parsing, alignment and output together according to
// the track graph's topological order, with bounded queues providing
// backpressure and a checkpoint store driving crash-safe resume.
package pipeline

import (
	"sync"

	"mediad/diag"
)

// BoundedQueue is a capacity-bounded channel wrapper with observability.
//
// When the queue is full the producer's Enqueue blocks until a consumer drains
// an item or the context is canceled: upstream reads pause rather than
// buffering without limit. Memory therefore scales with the configured
// capacity, never with input length. Peak depth is recorded for the demo's
// peak_queue/peak_memory_bytes assertions.
type BoundedQueue[T any] struct {
	ch  chan T
	cap int
	mu  sync.Mutex
	// peak is the largest depth reached; enqWait counts blocking enqueues
	// (backpressure events) for test/demo assertions.
	peak    int
	enqWait int64
}

// NewBoundedQueue validates capacity against the hard limits and returns a
// queue usable by many producers / one or more consumers.
func NewBoundedQueue[T any](capacity int) (*BoundedQueue[T], error) {
	if err := diag.DefaultLimits().CheckQueue(capacity); err != nil {
		return nil, err
	}
	return &BoundedQueue[T]{ch: make(chan T, capacity), cap: capacity}, nil
}

// Enqueue blocks while full. It returns an error if done closes while waiting
// (cancellation), releasing every blocked producer and consumer.
func (q *BoundedQueue[T]) Enqueue(v T, done <-chan struct{}) error {
	// Record a backpressure event before blocking; depth bookkeeping happens
	// after the item is actually handed to the channel.
	if len(q.ch) == q.cap {
		q.mu.Lock()
		q.enqWait++
		q.mu.Unlock()
	}
	select {
	case q.ch <- v:
		q.noteDepth()
		return nil
	case <-done:
		return diag.New(diag.EParam, "", "enqueue canceled")
	}
}

// Dequeue blocks until an item is available or done closes.
func (q *BoundedQueue[T]) Dequeue(done <-chan struct{}) (T, bool) {
	var zero T
	select {
	case v := <-q.ch:
		return v, true
	case <-done:
		return zero, false
	}
}

func (q *BoundedQueue[T]) noteDepth() {
	q.mu.Lock()
	if d := len(q.ch); d > q.peak {
		q.peak = d
	}
	q.mu.Unlock()
}

// Peak returns the largest depth reached so far.
func (q *BoundedQueue[T]) Peak() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// BackpressureEvents counts enqueues that found the queue full.
func (q *BoundedQueue[T]) BackpressureEvents() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.enqWait
}

// Len returns the current depth.
func (q *BoundedQueue[T]) Len() int { return len(q.ch) }

// Cap returns the configured capacity.
func (q *BoundedQueue[T]) Cap() int { return q.cap }
