// Package buffer provides the bounded byte-budgeted queue that sits between
// the RTP ingest stage and the audio processing stages. Producers block when
// the configured byte or item budget is exhausted (back-pressure); context
// cancellation and Close always release every waiting goroutine.
package buffer

import (
	"context"
	"sync"

	"mediamix/internal/clock"
)

// MaxQueueBytes is the hard per-queue memory ceiling (8 MiB).
const MaxQueueBytes = 8 << 20

// Window is a bounded queue limited both by item count and by total payload
// bytes. The zero value is not usable; construct it with NewWindow.
type Window[T any] struct {
	mu        sync.Mutex
	notFull   *sync.Cond
	notEmpty  *sync.Cond
	ring      []T
	head      int
	tail      int
	size      int
	bytes     int
	peakBytes int
	maxItems  int
	maxBytes  int
	closed    bool
	sizeOf    func(T) int
	watchOnce sync.Once
}

// NewWindow validates the limits and creates a queue. maxBytes must be
// between 1 and MaxQueueBytes; over-limit parameters are rejected rather
// than silently clamped so misconfiguration cannot blow the memory budget.
func NewWindow[T any](maxItems, maxBytes int, sizeOf func(T) int) (*Window[T], error) {
	if maxBytes <= 0 || maxBytes > MaxQueueBytes {
		return nil, clock.NewError(clock.StageBuffer, clock.ErrInvalidParam, 0, false,
			"queue byte budget must be within 1..8388608")
	}
	if maxItems <= 0 {
		return nil, clock.NewError(clock.StageBuffer, clock.ErrInvalidParam, 0, false,
			"queue item capacity must be positive")
	}
	if sizeOf == nil {
		sizeOf = func(T) int { return 1 }
	}
	w := &Window[T]{
		ring:     make([]T, maxItems),
		maxItems: maxItems,
		maxBytes: maxBytes,
		sizeOf:   sizeOf,
	}
	w.notFull = sync.NewCond(&w.mu)
	w.notEmpty = sync.NewCond(&w.mu)
	return w, nil
}

// watchContext starts (once) a goroutine that broadcasts both condition
// variables when ctx is cancelled, guaranteeing every waiter wakes up and
// re-evaluates its condition instead of sleeping forever.
func (w *Window[T]) watchContext(ctx context.Context) {
	w.watchOnce.Do(func() {
		go func() {
			<-ctx.Done()
			w.mu.Lock()
			defer w.mu.Unlock()
			w.notFull.Broadcast()
			w.notEmpty.Broadcast()
		}()
	})
}

// Put inserts an item, blocking while the queue is full. It returns
// ErrQueueClosed when the queue closes (or is already closed) and the
// context error wrapped in a locatable MediaError on cancellation. A single
// item larger than the whole byte budget is rejected rather than blocking.
func (w *Window[T]) Put(ctx context.Context, item T) error {
	cost := w.sizeOf(item)
	if cost > w.maxBytes {
		return clock.NewError(clock.StageBuffer, clock.ErrLimitExceeded, 0, false,
			"single item exceeds queue byte budget")
	}
	w.watchContext(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	for (w.size == w.maxItems || w.bytes+cost > w.maxBytes) && !w.closed {
		if err := ctx.Err(); err != nil {
			return clock.WrapError(clock.StageBuffer, clock.ErrTimeout, 0, true,
				"put cancelled while waiting for capacity", err)
		}
		w.notFull.Wait()
	}
	if err := ctx.Err(); err != nil {
		return clock.WrapError(clock.StageBuffer, clock.ErrTimeout, 0, true,
			"put cancelled", err)
	}
	if w.closed {
		return clock.NewError(clock.StageBuffer, clock.ErrQueueClosed, 0, false, "put on closed queue")
	}
	w.ring[w.tail] = item
	w.tail = (w.tail + 1) % w.maxItems
	w.size++
	w.bytes += cost
	if w.bytes > w.peakBytes {
		w.peakBytes = w.bytes
	}
	w.notEmpty.Signal()
	return nil
}

// Get removes and returns the oldest item, blocking while the queue is
// empty. After Close, queued items are still delivered; once drained Get
// returns ErrQueueClosed.
func (w *Window[T]) Get(ctx context.Context) (T, error) {
	var zero T
	w.watchContext(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.size == 0 && !w.closed {
		if err := ctx.Err(); err != nil {
			return zero, clock.WrapError(clock.StageBuffer, clock.ErrTimeout, 0, true,
				"get cancelled while waiting for items", err)
		}
		w.notEmpty.Wait()
	}
	if w.size == 0 {
		return zero, clock.NewError(clock.StageBuffer, clock.ErrQueueClosed, 0, false, "queue drained and closed")
	}
	item := w.ring[w.head]
	w.ring[w.head] = zero
	w.head = (w.head + 1) % w.maxItems
	w.size--
	w.bytes -= w.sizeOf(item)
	w.notFull.Signal()
	return item, nil
}

// Close marks the queue closed and releases all waiters. It is idempotent.
// Items already queued remain readable via Get until drained.
func (w *Window[T]) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	w.notFull.Broadcast()
	w.notEmpty.Broadcast()
}

// PeakBytes returns the highest byte occupancy observed so far.
func (w *Window[T]) PeakBytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peakBytes
}

// Bytes returns the current queued payload bytes.
func (w *Window[T]) Bytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

// Len returns the current queued item count.
func (w *Window[T]) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// Closed reports whether the queue has been closed.
func (w *Window[T]) Closed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}
