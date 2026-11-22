// Package stream connects processing stages with bounded queues. Each
// stage limits both item count and buffered bytes; a slow consumer
// blocks its producer (backpressure), and cancellation or a sink
// failure drains queues and reports the last committed boundary.
package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrStageStopped is returned to producers after the pipeline aborted.
var ErrStageStopped = errors.New("stream: stage stopped, buffer released")

// Sizer reports the byte cost of a queued item.
type Sizer[T any] func(T) int

// Queue is a bounded, byte-budgeted blocking queue.
type Queue[T any] struct {
	ch       chan T
	maxItems int
	maxBytes int64
	sizeOf   Sizer[T]

	mu      sync.Mutex
	cond    *sync.Cond
	bytes   int64
	peak    int64
	stopped bool
	closed  bool
	stopCh  chan struct{}
}

// NewQueue creates a queue with item and byte budgets.
func NewQueue[T any](maxItems int, maxBytes int64, sizeOf Sizer[T]) (*Queue[T], error) {
	if maxItems <= 0 || maxBytes <= 0 {
		return nil, fmt.Errorf("stream: budgets must be positive (items=%d bytes=%d)", maxItems, maxBytes)
	}
	q := &Queue[T]{
		ch:       make(chan T, maxItems),
		maxItems: maxItems,
		maxBytes: maxBytes,
		sizeOf:   sizeOf,
		stopCh:   make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mu)
	return q, nil
}

// Put blocks while the byte or item budget is full. On stop/cancel it
// releases and returns ErrStageStopped instead of buffering forever.
func (q *Queue[T]) Put(ctx context.Context, item T) error {
	size := int64(q.sizeOf(item))
	if size > q.maxBytes {
		return fmt.Errorf("stream: item of %d bytes exceeds queue budget %d", size, q.maxBytes)
	}

	// Wake the condition waiter when the context is cancelled.
	ctxDone := make(chan struct{})
	defer close(ctxDone)
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast()
		case <-ctxDone:
		case <-q.stopCh:
			q.cond.Broadcast()
		}
	}()

	// Reserve budget under the lock, then perform the blocking send
	// outside it so Takes can still drain and broadcast progress.
	q.mu.Lock()
	for !q.fitsLocked(size) {
		if q.stopped || q.closed {
			q.mu.Unlock()
			return ErrStageStopped
		}
		if err := ctx.Err(); err != nil {
			q.mu.Unlock()
			return err
		}
		q.cond.Wait()
	}
	if q.stopped || q.closed {
		q.mu.Unlock()
		return ErrStageStopped
	}
	// Commit the reservation immediately; Take reverses it if needed.
	q.bytes += size
	if q.bytes > q.peak {
		q.peak = q.bytes
	}
	q.mu.Unlock()

	select {
	case q.ch <- item:
		return nil
	case <-ctx.Done():
		q.rollback(size)
		return ctx.Err()
	case <-q.stopCh:
		q.rollback(size)
		return ErrStageStopped
	}
}

// rollback undoes a budget reservation when a send never completed.
func (q *Queue[T]) rollback(size int64) {
	q.mu.Lock()
	q.bytes -= size
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *Queue[T]) fitsLocked(size int64) bool {
	return len(q.ch) < q.maxItems && q.bytes+size <= q.maxBytes
}

// Take blocks until an item is available.
func (q *Queue[T]) Take(ctx context.Context) (T, bool, error) {
	var zero T
	select {
	case item, ok := <-q.ch:
		if !ok {
			return zero, false, nil
		}
		q.mu.Lock()
		q.bytes -= int64(q.sizeOf(item))
		q.cond.Broadcast() // a waiting producer may now fit
		q.mu.Unlock()
		return item, true, nil
	case <-ctx.Done():
		return zero, false, ctx.Err()
	case <-q.stopCh:
		return zero, false, ErrStageStopped
	}
}

// Stop releases all waiters; buffered items are dropped with the queue.
func (q *Queue[T]) Stop() {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	q.stopped = true
	close(q.stopCh)
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Close signals normal completion: no further Puts are accepted, but
// buffered items can still be drained via Take until the channel empties.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped || q.closed {
		return
	}
	q.closed = true
	close(q.ch)
	q.cond.Broadcast()
}

// PeakBytes reports the high-water mark of buffered bytes.
func (q *Queue[T]) PeakBytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.peak
}

// Bytes reports the currently buffered byte count.
func (q *Queue[T]) Bytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

// Pipeline wires a chain of queues and tracks aggregate peak bytes.
type Pipeline[T any] struct {
	queues []*Queue[T]
	cancel context.CancelFunc
}

// NewPipeline builds n chained bounded queues sharing one context.
func NewPipeline[T any](ctx context.Context, stages int, maxItems int, maxBytes int64, sizeOf Sizer[T]) (*Pipeline[T], context.Context, error) {
	if stages < 1 {
		return nil, nil, fmt.Errorf("stream: pipeline needs at least one stage, got %d", stages)
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Pipeline[T]{cancel: cancel}
	for i := 0; i < stages; i++ {
		q, err := NewQueue[T](maxItems, maxBytes, sizeOf)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		p.queues = append(p.queues, q)
	}
	return p, ctx, nil
}

// Queue returns the i-th stage queue.
func (p *Pipeline[T]) Queue(i int) *Queue[T] { return p.queues[i] }

// Abort stops every stage, releasing buffered data and waiters.
func (p *Pipeline[T]) Abort() {
	for _, q := range p.queues {
		q.Stop()
	}
	p.cancel()
}

// PeakBytes sums the high-water marks of all stages.
func (p *Pipeline[T]) PeakBytes() int64 {
	var sum int64
	for _, q := range p.queues {
		sum += q.PeakBytes()
	}
	return sum
}
