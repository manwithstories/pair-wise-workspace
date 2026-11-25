// Package stream provides bounded queues with backpressure. Producers block
// when the byte budget is exhausted, consumers signal completion or error,
// and cancelling a job releases every queued item promptly.
package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"mediapipe/internal/media"
)

// DefaultBudget is the delivery contract's 32 MiB ceiling.
const DefaultBudget int64 = 32 << 20

// Queue is a bounded MPMC queue measured in bytes. Byte accounting is the
// admission control: a push that would exceed the budget blocks until a
// consumer frees room, the context is cancelled, or the queue is closed.
type Queue[T any] struct {
	mu       sync.Mutex
	notFull  *sync.Cond
	notEmpty *sync.Cond

	budget    int64
	size      int64
	peak      int64
	items     []T
	head      int
	closed    bool
	cancelled bool
	// sizing returns the byte cost of an item.
	sizing func(T) int64
	// dropped counts items discarded by cancellation.
	dropped int64
	// pushes and pops feed deterministic statistics.
	pushes int64
	pops   int64
	// waiters counts blocked producers and consumers for diagnostics.
	waiters int32
}

// NewQueue returns a queue with the given byte budget. sizing must return a
// non-negative cost per item; nil means every item costs one byte.
func NewQueue[T any](budget int64, sizing func(T) int64) *Queue[T] {
	if budget <= 0 {
		budget = DefaultBudget
	}
	if sizing == nil {
		sizing = func(T) int64 { return 1 }
	}
	q := &Queue[T]{budget: budget, sizing: sizing}
	q.notFull = sync.NewCond(&q.mu)
	q.notEmpty = sync.NewCond(&q.mu)
	return q
}

// Push enqueues item, blocking while the byte budget is full. It returns
// context.Canceled when ctx is cancelled and ErrClosed after Close.
func (q *Queue[T]) Push(ctx context.Context, item T) error {
	cost := q.sizing(item)
	if cost < 0 {
		cost = 0
	}
	if cost > q.budget {
		return media.NewError("stream.push", media.ClassFatal,
			errTooLarge(cost, q.budget))
	}
	// Wake the consumer when the context is cancelled so Push can observe it.
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.notFull.Broadcast()
		q.notEmpty.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	atomic.AddInt32(&q.waiters, 1)
	defer atomic.AddInt32(&q.waiters, -1)
	for {
		if q.cancelled {
			return context.Canceled
		}
		if q.closed {
			return ErrClosed
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if q.size+cost <= q.budget {
			break
		}
		q.notFull.Wait()
	}
	q.items = append(q.items, item)
	q.size += cost
	q.pushes++
	if q.size > q.peak {
		q.peak = q.size
	}
	q.notEmpty.Signal()
	return nil
}

// Pop removes the oldest item, blocking until one is available. It returns
// ErrClosed once the queue is drained and closed.
func (q *Queue[T]) Pop(ctx context.Context) (T, error) {
	var zero T
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.notFull.Broadcast()
		q.notEmpty.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.cancelled {
			return zero, context.Canceled
		}
		if q.head < len(q.items) {
			item := q.items[q.head]
			q.items[q.head] = zero // release references for GC
			q.head++
			if q.head == len(q.items) {
				q.items = q.items[:0]
				q.head = 0
			}
			q.size -= q.sizing(item)
			if q.size < 0 {
				q.size = 0
			}
			q.pops++
			q.notFull.Signal()
			return item, nil
		}
		if q.closed {
			return zero, ErrClosed
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		q.notEmpty.Wait()
	}
}

// Close stops further pushes. Buffered items remain available to Pop.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.notEmpty.Broadcast()
		q.notFull.Broadcast()
	}
	q.mu.Unlock()
}

// Cancel releases every buffered item and unblocks all waiters. It is safe to
// call more than once.
func (q *Queue[T]) Cancel() {
	q.mu.Lock()
	if !q.cancelled {
		q.cancelled = true
		atomic.AddInt64(&q.dropped, int64(len(q.items)-q.head))
		q.size = 0
		q.items = nil
		q.head = 0
		q.notFull.Broadcast()
		q.notEmpty.Broadcast()
	}
	q.mu.Unlock()
}

// ErrClosed is returned when pushing to or popping from a closed queue.
var ErrClosed = media.NewError("stream.queue", media.ClassFatal, queueError("queue closed"))

type queueError string

func (e queueError) Error() string { return string(e) }

// ErrOversize marks a single item larger than the whole budget.
var ErrOversize = errors.New("item exceeds queue budget")

func errTooLarge(cost, budget int64) error {
	return media.NewError("stream.push", media.ClassFatal,
		fmt.Errorf("%w: %d > %d", ErrOversize, cost, budget))
}

// Stats is a point-in-time view of queue usage.
type Stats struct {
	Budget  int64
	Bytes   int64
	Peak    int64
	Items   int
	Pushes  int64
	Pops    int64
	Dropped int64
	Closed  bool
}

// Stats returns current accounting.
func (q *Queue[T]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{
		Budget:  q.budget,
		Bytes:   q.size,
		Peak:    q.peak,
		Items:   len(q.items) - q.head,
		Pushes:  q.pushes,
		Pops:    q.pops,
		Dropped: atomic.LoadInt64(&q.dropped),
		Closed:  q.closed,
	}
}

// Peak returns the high-water mark of buffered bytes.
func (q *Queue[T]) Peak() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// Len returns the number of buffered items.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) - q.head
}

// ResetPeak clears the high-water mark; used between demo cases so each case
// reports its own peak.
func (q *Queue[T]) ResetPeak() {
	q.mu.Lock()
	q.peak = q.size
	q.mu.Unlock()
}

// PacketSizer returns a sizing function for media packets that charges the
// payload length plus a fixed per-packet overhead.
func PacketSizer(overhead int64) func(media.Packet) int64 {
	return func(p media.Packet) int64 { return int64(len(p.Data)) + overhead }
}
