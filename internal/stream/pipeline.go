// Package stream wires pipeline stages with bounded queues so slow
// consumers apply backpressure instead of growing memory without limit.
package stream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrSampleTooLarge rejects items above the configured per-item byte cap.
var ErrSampleTooLarge = errors.New("sample exceeds maximum size")

// ErrQueueClosed is returned when pushing to a closed queue.
var ErrQueueClosed = errors.New("queue closed")

// Queue is a bounded, closeable channel with instrumentation. Occupancy is
// governed by a slot semaphore, so the observed peak never exceeds the
// configured capacity.
type Queue[T any] struct {
	ch       chan T
	slots    chan struct{}
	maxBytes int
	closed   atomic.Bool
	cur      atomic.Int64
	peak     atomic.Int64
	blocked  atomic.Int64 // push operations that had to wait
}

// NewQueue creates a queue with the given item capacity and per-item byte
// cap (0 = unlimited).
func NewQueue[T any](capacity, maxBytes int) *Queue[T] {
	if capacity < 1 {
		capacity = 1
	}
	q := &Queue[T]{
		ch:       make(chan T, capacity),
		slots:    make(chan struct{}, capacity),
		maxBytes: maxBytes,
	}
	for i := 0; i < capacity; i++ {
		q.slots <- struct{}{}
	}
	return q
}

// Push enqueues item. It blocks while the queue is full (backpressure) or
// until ctx is cancelled. sizeBytes is charged against the byte cap.
func (q *Queue[T]) Push(ctx context.Context, item T, sizeBytes int) error {
	if q.maxBytes > 0 && sizeBytes > q.maxBytes {
		return ErrSampleTooLarge
	}
	if q.closed.Load() {
		return ErrQueueClosed
	}
	// Non-blocking attempt first so we only count real backpressure.
	select {
	case <-q.slots:
	default:
		q.blocked.Add(1)
		select {
		case <-q.slots:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	q.ch <- item
	c := q.cur.Add(1)
	for {
		p := q.peak.Load()
		if c <= p || q.peak.CompareAndSwap(p, c) {
			break
		}
	}
	return nil
}

// Pop dequeues one item; ok is false once the queue is closed and drained.
func (q *Queue[T]) Pop() (item T, ok bool) {
	item, ok = <-q.ch
	if ok {
		q.cur.Add(-1)
		q.slots <- struct{}{}
	}
	return item, ok
}

// Close signals no further pushes; buffered items remain readable.
func (q *Queue[T]) Close() {
	if q.closed.CompareAndSwap(false, true) {
		close(q.ch)
	}
}

// Peak returns the maximum observed occupancy (never above capacity).
func (q *Queue[T]) Peak() int { return int(q.peak.Load()) }

// BackpressureEvents returns how many pushes had to wait for capacity.
func (q *Queue[T]) BackpressureEvents() int { return int(q.blocked.Load()) }

// Stage transforms input items into output items.
type Stage[In, Out any] func(ctx context.Context, in In, emit func(Out) error) error

// Pipeline runs a producer and a consumer connected by one bounded queue.
// Cancelling ctx stops the producer; items already queued are drained by
// the consumer, so completed work is kept.
type Pipeline struct {
	Capacity int
	MaxBytes int
}

// NewPipeline builds a pipeline with the given queue capacity and per-item
// byte cap.
func NewPipeline(capacity, maxBytes int) *Pipeline {
	return &Pipeline{Capacity: capacity, MaxBytes: maxBytes}
}

// QueueLike is the subset of Queue used by Run.
type QueueLike interface{ Close() }

// Run executes produce and consume concurrently, closing the queue when the
// producer returns. It propagates the first non-cancellation error.
func (p *Pipeline) Run(ctx context.Context, q QueueLike, produce func(ctx context.Context) error, consume func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer q.Close()
		defer cancel()
		errs[0] = produce(ctx)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		errs[1] = consume(ctx)
	}()
	wg.Wait()
	for _, e := range errs {
		if e != nil && !errors.Is(e, context.Canceled) {
			return e
		}
	}
	return nil
}
