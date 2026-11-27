// Package pipeline connects the processing stages with bounded,
// cancellation-aware queues.  The queue is the only place that buffers, so
// the hard limits on in-flight events and bytes are enforced in one place.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"mediacheck/internal/model"
)

// ErrQueueClosed is returned by Push after Close.
var ErrQueueClosed = errors.New("pipeline: queue closed")

// Queue is a bounded FIFO of packet events with both an item and a byte
// budget.  Push blocks while either bound would be exceeded, which is how a
// slow consumer pauses the upstream read: the reader's EmitFunc calls Push,
// so a full queue stops the reader instead of buffering without limit.
//
// A context bound with Bind runs a watcher that wakes every waiter and marks
// the queue cancelled, so a cancelled run never leaves a goroutine parked in
// Wait.  Close instead performs a graceful shutdown: pushes fail and Pop
// drains what is already queued.
type Queue struct {
	mu       sync.Mutex
	notFull  *sync.Cond
	notEmpty *sync.Cond

	items    []model.Packet
	bytes    int64
	maxItems int
	maxBytes int64

	closed   bool
	canceled error

	peakItems int
	peakBytes int64
	pushed    int64
	popped    int64
}

// NewQueue returns a queue holding at most maxItems packets and maxBytes of
// payload.  Both bounds are clamped to at least one so a single oversized
// packet cannot deadlock the producer.
func NewQueue(maxItems int, maxBytes int64) *Queue {
	if maxItems <= 0 {
		maxItems = 1
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	q := &Queue{
		items:    make([]model.Packet, 0, maxItems),
		maxItems: maxItems,
		maxBytes: maxBytes,
	}
	q.notFull = sync.NewCond(&q.mu)
	q.notEmpty = sync.NewCond(&q.mu)
	return q
}

// Bind ties the queue to ctx.  When ctx is done every blocked Push and Pop
// returns a wrapped ErrCanceled.  The watcher exits with the queue, so a
// long-lived queue does not leak a goroutine per bind.
func (q *Queue) Bind(ctx context.Context) {
	if ctx == nil || ctx.Done() == nil {
		return
	}
	go func() {
		<-ctx.Done()
		q.mu.Lock()
		if q.canceled == nil {
			q.canceled = ctx.Err()
		}
		q.mu.Unlock()
		q.notFull.Broadcast()
		q.notEmpty.Broadcast()
	}()
}

// Push appends p, blocking while either bound would be exceeded.
func (q *Queue) Push(p model.Packet) error {
	size := int64(len(p.Data))
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed && q.canceled == nil && (len(q.items) >= q.maxItems || q.bytes+size > q.maxBytes) {
		q.notFull.Wait()
	}
	if err := q.errLocked(); err != nil {
		return err
	}
	q.items = append(q.items, p)
	q.bytes += size
	q.pushed++
	if len(q.items) > q.peakItems {
		q.peakItems = len(q.items)
	}
	if q.bytes > q.peakBytes {
		q.peakBytes = q.bytes
	}
	q.notEmpty.Signal()
	return nil
}

// Pop removes the oldest packet, blocking while the queue is empty.  The
// boolean is false once the queue is closed and drained.
func (q *Queue) Pop() (model.Packet, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed && q.canceled == nil {
		q.notEmpty.Wait()
	}
	if q.canceled != nil {
		return model.Packet{}, false, fmt.Errorf("%w: %v", model.ErrCanceled, q.canceled)
	}
	if len(q.items) == 0 {
		return model.Packet{}, false, nil
	}
	p := q.items[0]
	// Keep the backing array bounded: shift in place and shrink.
	copy(q.items, q.items[1:])
	q.items = q.items[:len(q.items)-1]
	q.bytes -= int64(len(p.Data))
	q.popped++
	q.notFull.Signal()
	return p, true, nil
}

func (q *Queue) errLocked() error {
	if q.canceled != nil {
		return fmt.Errorf("%w: %v", model.ErrCanceled, q.canceled)
	}
	if q.closed {
		return ErrQueueClosed
	}
	return nil
}

// Close performs a graceful shutdown: further pushes fail, queued items can
// still be popped, and every waiter wakes.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.notFull.Broadcast()
	q.notEmpty.Broadcast()
}

// Len returns the number of queued packets.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Bytes returns the queued payload size.
func (q *Queue) Bytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

// QueueStats is a point-in-time summary for the report.
type QueueStats struct {
	PeakItems int
	PeakBytes int64
	Pushed    int64
	Popped    int64
	MaxItems  int
	MaxBytes  int64
}

// Stats returns the counters.
func (q *Queue) Stats() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QueueStats{
		PeakItems: q.peakItems,
		PeakBytes: q.peakBytes,
		Pushed:    q.pushed,
		Popped:    q.popped,
		MaxItems:  q.maxItems,
		MaxBytes:  q.maxBytes,
	}
}

// WithinBounds reports whether the observed peak stayed inside the configured
// item bound.  The demo asserts this.
func (q *Queue) WithinBounds() bool {
	st := q.Stats()
	return st.PeakItems <= st.MaxItems
}
