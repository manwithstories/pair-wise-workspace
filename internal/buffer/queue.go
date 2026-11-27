// Package buffer provides a byte-bounded FIFO queue with context-aware
// backpressure. Producers block (rather than allocate) when the memory
// ceiling is reached, and cancellation always releases waiters while
// leaving already-enqueued work drainable for progress accounting.
package buffer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"archive/internal/model"
)

// ErrQueueClosed is returned by Push after Close and by Pop once the queue
// has been closed and fully drained.
var ErrQueueClosed = errors.New("queue closed")

type qItem[T any] struct {
	v    T
	size int
}

// Queue is a generic bounded FIFO. Memory is measured in bytes through the
// caller-supplied sizer; the queue never exceeds maxBytes of live items.
type Queue[T any] struct {
	mu       sync.Mutex
	cond     *sync.Cond
	items    []qItem[T]
	bytes    int
	maxBytes int
	peak     int
	closed   bool
	sizer    func(T) int
	log      *model.FaultLog
	waits    int
}

// NewQueue builds a queue capped at maxBytes. A non-positive cap falls back
// to model.MaxQueueBytes; caps above it are rejected by the caller instead,
// so this constructor clamps rather than silently enlarging the budget.
func NewQueue[T any](maxBytes int, sizer func(T) int, log *model.FaultLog) *Queue[T] {
	if maxBytes <= 0 || maxBytes > model.MaxQueueBytes {
		maxBytes = model.MaxQueueBytes
	}
	q := &Queue[T]{maxBytes: maxBytes, sizer: sizer, log: log}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Push appends one item, blocking while the byte ceiling is occupied. A
// single oversize item is rejected immediately rather than deadlocking the
// queue. Context cancellation wakes the waiter and leaves the queue intact.
func (q *Queue[T]) Push(ctx context.Context, v T) error {
	size := q.sizer(v)
	if size <= 0 {
		return model.InvariantError(model.PhaseBuffer, "non-positive item size %d", size)
	}
	if size > q.maxBytes {
		q.log.Add(model.Fault{Phase: model.PhaseBuffer, Kind: model.FaultOversize,
			Detail: fmt.Sprintf("item %d bytes exceeds queue cap %d", size, q.maxBytes)})
		return model.InvariantError(model.PhaseBuffer, "item exceeds queue capacity")
	}

	stop := context.AfterFunc(ctx, q.broadcast)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()

	blocked := false
	for !q.closed && q.bytes+size > q.maxBytes {
		if !blocked {
			blocked = true
			q.waits++
			q.log.Add(model.Fault{Phase: model.PhaseBuffer, Kind: model.FaultBackpressure,
				Detail: fmt.Sprintf("upstream paused, %d/%d bytes used", q.bytes, q.maxBytes)})
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		q.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.closed {
		return ErrQueueClosed
	}
	q.items = append(q.items, qItem[T]{v: v, size: size})
	q.bytes += size
	if q.bytes > q.peak {
		q.peak = q.bytes
	}
	q.cond.Broadcast()
	return nil
}

// Pop removes and returns the head item, blocking while empty. After Close
// it keeps returning enqueued items, then returns ErrQueueClosed.
func (q *Queue[T]) Pop(ctx context.Context) (T, error) {
	var zero T
	stop := context.AfterFunc(ctx, q.broadcast)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		q.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if len(q.items) == 0 {
		return zero, ErrQueueClosed
	}
	head := q.items[0]
	q.items = q.items[1:]
	q.bytes -= head.size
	q.cond.Broadcast()
	return head.v, nil
}

// Close marks the queue closed and releases every blocked waiter. Items
// already enqueued remain available through Pop and DrainAll, so shutdown
// preserves determinable progress instead of discarding work.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// DrainAll removes and returns every remaining item in FIFO order. Used by
// shutdown paths to account for buffered-but-unprocessed work.
func (q *Queue[T]) DrainAll() []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]T, len(q.items))
	for i, it := range q.items {
		out[i] = it.v
	}
	q.items = nil
	q.bytes = 0
	q.cond.Broadcast()
	return out
}

// Len reports the number of queued items.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Bytes reports the live byte occupancy of the queue.
func (q *Queue[T]) Bytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

// PeakBytes reports the highest occupancy observed since construction.
func (q *Queue[T]) PeakBytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// BackpressureEvents reports how many Push calls had to block.
func (q *Queue[T]) BackpressureEvents() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.waits
}

func (q *Queue[T]) broadcast() {
	q.mu.Lock()
	q.cond.Broadcast()
	q.mu.Unlock()
}
