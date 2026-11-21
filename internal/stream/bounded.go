// Package stream provides a bounded, cancellable queue between the demuxer
// and the remuxer. Peak memory is bounded by capacity times the largest item;
// a slow consumer blocks producers instead of growing a backlog, and
// cancellation is propagated to every participant.
package stream

import (
	"context"
	"errors"
	"sync"

	"archivecheck.local/archivecheck/internal/fault"
)

// ErrClosed is returned when sending on a closed queue.
var ErrClosed = errors.New("stream: queue closed")

// Queue is a fixed-capacity FIFO ring. Occupancy is guarded by a mutex, which
// makes the peak water mark exact and keeps producers blocked (rather than
// buffering) when the consumer is slow.
type Queue[T any] struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond

	buf    []T
	head   int
	size   int
	closed bool
	cause  error
	done   chan struct{}

	peak     int
	sent     int64
	received int64
	blocked  int64
}

// Stats reports queue behaviour.
type Stats struct {
	Capacity int
	Peak     int
	Sent     int64
	Received int64
	// Blocked counts sends that had to wait for space.
	Blocked int64
}

// New creates a queue of the given capacity. A non-positive capacity is an
// error: an unbounded queue would defeat the memory guarantee.
func New[T any](capacity int) (*Queue[T], error) {
	if capacity <= 0 {
		return nil, errors.New("stream: capacity must be positive")
	}
	q := &Queue[T]{buf: make([]T, capacity), done: make(chan struct{})}
	q.notEmpty = sync.NewCond(&q.mu)
	q.notFull = sync.NewCond(&q.mu)
	return q, nil
}

// Send enqueues v. It blocks while the queue is full, and returns the context
// error if ctx is cancelled first. The error carries the queue stage and, when
// the queue was closed, the cause supplied by CloseWith.
func (q *Queue[T]) Send(ctx context.Context, v T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size == len(q.buf) {
		q.blocked++
	}
	for q.size == len(q.buf) && !q.closed {
		if err := ctx.Err(); err != nil {
			return fault.New(fault.StageQueue, err)
		}
		q.wait(ctx, q.notFull)
	}
	if err := ctx.Err(); err != nil {
		return fault.New(fault.StageQueue, err)
	}
	if q.closed {
		return fault.New(fault.StageQueue, q.closeCauseLocked())
	}
	q.buf[(q.head+q.size)%len(q.buf)] = v
	q.size++
	if q.size > q.peak {
		q.peak = q.size
	}
	q.sent++
	q.notEmpty.Broadcast()
	return nil
}

// wait blocks on c until it is broadcast, the context is cancelled, or the
// queue is closed. q.mu must be held; it remains held on return.
func (q *Queue[T]) wait(ctx context.Context, c *sync.Cond) {
	if ctx.Done() == nil {
		c.Wait()
		return
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			q.mu.Lock()
			c.Broadcast()
			q.mu.Unlock()
		case <-stop:
		}
	}()
	c.Wait()
	// Release the queue lock before joining the watcher: the watcher may be
	// waiting to acquire the same lock.
	q.mu.Unlock()
	close(stop)
	<-finished
	q.mu.Lock()
}

// Recv dequeues one item. ok is false when the queue is closed and drained.
func (q *Queue[T]) Recv() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.size == 0 && !q.closed {
		q.notEmpty.Wait()
	}
	return q.popLocked()
}

// RecvCtx dequeues one item, returning a queue-stage error when ctx is
// cancelled or the queue is closed. Cancellation also closes the queue so the
// producing side unblocks immediately.
func (q *Queue[T]) RecvCtx(ctx context.Context) (T, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.size == 0 && !q.closed {
		if err := ctx.Err(); err != nil {
			q.closeLocked(err)
			var zero T
			return zero, fault.New(fault.StageQueue, err)
		}
		q.wait(ctx, q.notEmpty)
	}
	if q.size == 0 {
		var zero T
		return zero, fault.New(fault.StageQueue, q.closeCauseLocked())
	}
	v, _ := q.popLocked()
	return v, nil
}

func (q *Queue[T]) popLocked() (T, bool) {
	var zero T
	if q.size == 0 {
		return zero, false
	}
	v := q.buf[q.head]
	q.buf[q.head] = zero
	q.head = (q.head + 1) % len(q.buf)
	q.size--
	q.received++
	q.notFull.Broadcast()
	return v, true
}

// Close closes the queue. Buffered items can still be received until drained.
func (q *Queue[T]) Close() { q.CloseWith(nil) }

// CloseWith closes the queue and records cause as the cancellation reason.
func (q *Queue[T]) CloseWith(cause error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closeLocked(cause)
}

func (q *Queue[T]) closeLocked(cause error) {
	if q.closed {
		return
	}
	q.closed = true
	if cause == nil {
		cause = ErrClosed
	}
	q.cause = cause
	close(q.done)
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
}

func (q *Queue[T]) closeCause() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closeCauseLocked()
}

func (q *Queue[T]) closeCauseLocked() error {
	if q.cause != nil {
		return q.cause
	}
	return ErrClosed
}

// Len returns the current occupancy.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.size
}

// Cap returns the fixed capacity.
func (q *Queue[T]) Cap() int { return len(q.buf) }

// Stats returns a snapshot of queue statistics.
func (q *Queue[T]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{
		Capacity: len(q.buf),
		Peak:     q.peak,
		Sent:     q.sent,
		Received: q.received,
		Blocked:  q.blocked,
	}
}

// Done returns a channel closed when the queue is closed.
func (q *Queue[T]) Done() <-chan struct{} { return q.done }
