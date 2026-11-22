// Package stream connects the pipeline stages with bounded in-memory
// queues. A queue is limited by both total queued bytes and in-flight
// item count; when either limit is reached, producers block instead of
// growing memory without bound. All blocking is context-aware and every
// enqueue/dequeue releases cleanly on cancellation.
package stream

import (
	"context"
	"sync"

	"github.com/clearliu/fieldcut/internal/clock"
)

// Hard caps defend against absurd configuration values.
const (
	MaxQueueBytes = 512 << 20 // 512 MiB absolute ceiling
	MaxQueueItems = 1_000_000
)

// Item is one queued work unit. Size drives the byte budget; the payload
// is opaque to the queue. TrackID/Sequence let checkpoints describe what
// is still in flight without copying the payload.
type Item struct {
	TrackID  int
	Sequence int
	Size     int
	Payload  any
}

// QueueMetrics is a point-in-time queue snapshot.
type QueueMetrics struct {
	QueuedBytes int
	QueuedItems int
	PeakBytes   int
	TotalIn     int64
	TotalOut    int64
	Dropped     int64
}

// Checkpoint is a resumable queue position. It stores descriptors only,
// never payloads, so on cancellation/failure the caller can release all
// buffered memory and restart from the last committed position.
type Checkpoint struct {
	QueuedBytes int
	QueuedItems int
	Pending     []ItemDescriptor
}

// ItemDescriptor identifies one queued item without its payload.
type ItemDescriptor struct {
	TrackID  int
	Sequence int
	Size     int
}

// BoundedQueue is a FIFO queue with hard byte and item budgets. The zero
// value is not usable; construct with NewBoundedQueue.
type BoundedQueue struct {
	mu        sync.Mutex
	cond      *sync.Cond
	maxBytes  int
	maxItems  int
	items     []Item
	bytes     int
	peakBytes int
	closed    bool
	totalIn   int64
	totalOut  int64
	dropped   int64
}

// NewBoundedQueue validates and caps the budgets. Non-positive values
// fall back to safe defaults.
func NewBoundedQueue(maxBytes, maxItems int) *BoundedQueue {
	if maxBytes <= 0 {
		maxBytes = 8 << 20 // 8 MiB
	}
	if maxBytes > MaxQueueBytes {
		maxBytes = MaxQueueBytes
	}
	if maxItems <= 0 {
		maxItems = 1024
	}
	if maxItems > MaxQueueItems {
		maxItems = MaxQueueItems
	}
	q := &BoundedQueue{
		maxBytes: maxBytes,
		maxItems: maxItems,
		items:    make([]Item, 0, min(maxItems, 4096)),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue blocks while the byte or item budget is exhausted. A cancelled
// context returns ctx.Err() without touching the queue; Close causes
// clock.ErrQueueClosed. Backpressure is observable to the caller only as
// blocking time, which is exactly what the slow-consumer demo measures.
func (q *BoundedQueue) Enqueue(ctx context.Context, it Item) error {
	if it.Size < 0 {
		return clock.ErrInvalidArgument
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed {
		if q.bytes+it.Size <= q.maxBytes && len(q.items) < q.maxItems {
			q.items = append(q.items, it)
			q.bytes += it.Size
			q.totalIn++
			if q.bytes > q.peakBytes {
				q.peakBytes = q.bytes
			}
			q.cond.Broadcast()
			return nil
		}
		if err := q.wait(ctx); err != nil {
			return err
		}
	}
	return clock.ErrQueueClosed
}

// TryEnqueue is the non-blocking variant: it returns clock.ErrQueueFull
// immediately when a budget is exhausted instead of applying backpressure.
func (q *BoundedQueue) TryEnqueue(it Item) error {
	if it.Size < 0 {
		return clock.ErrInvalidArgument
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return clock.ErrQueueClosed
	}
	if q.bytes+it.Size > q.maxBytes || len(q.items) >= q.maxItems {
		q.dropped++
		return clock.ErrQueueFull
	}
	q.items = append(q.items, it)
	q.bytes += it.Size
	q.totalIn++
	if q.bytes > q.peakBytes {
		q.peakBytes = q.bytes
	}
	q.cond.Broadcast()
	return nil
}

// Dequeue blocks until an item is available. On Close with an empty queue
// it returns (zero, false, nil) so consumer loops terminate cleanly.
func (q *BoundedQueue) Dequeue(ctx context.Context) (Item, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		if q.closed {
			return Item{}, false, nil
		}
		if err := q.wait(ctx); err != nil {
			return Item{}, false, err
		}
	}
	it := q.items[0]
	q.items = q.items[1:]
	q.bytes -= it.Size
	q.totalOut++
	q.cond.Broadcast()
	return it, true, nil
}

// wait suspends until Broadcast or context cancellation. The caller must
// hold q.mu; sync.Cond releases it while waiting and reacquires after.
func (q *BoundedQueue) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast()
		case <-done:
		}
	}()
	q.cond.Wait()
	close(done)
	return ctx.Err()
}

// Close rejects further enqueues and wakes every waiter. Already queued
// items remain drainable; call DrainAndRelease to free their memory.
func (q *BoundedQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// DrainAndRelease empties the queue and returns its byte total, used on
// cancellation/failure paths to prove all buffered memory is released.
func (q *BoundedQueue) DrainAndRelease() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	released := q.bytes
	q.items = nil
	q.bytes = 0
	q.cond.Broadcast()
	return released
}

// Snapshot returns a copy of the current metrics.
func (q *BoundedQueue) Snapshot() QueueMetrics {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QueueMetrics{
		QueuedBytes: q.bytes,
		QueuedItems: len(q.items),
		PeakBytes:   q.peakBytes,
		TotalIn:     q.totalIn,
		TotalOut:    q.totalOut,
		Dropped:     q.dropped,
	}
}

// Checkpoint captures descriptors of everything still in flight.
func (q *BoundedQueue) Checkpoint() Checkpoint {
	q.mu.Lock()
	defer q.mu.Unlock()
	cp := Checkpoint{
		QueuedBytes: q.bytes,
		QueuedItems: len(q.items),
		Pending:     make([]ItemDescriptor, len(q.items)),
	}
	for i, it := range q.items {
		cp.Pending[i] = ItemDescriptor{TrackID: it.TrackID, Sequence: it.Sequence, Size: it.Size}
	}
	return cp
}

// Resume verifies a checkpoint against current queue capacity; it does
// not rehydrate payloads (the producer re-sends them) but guarantees the
// pending set fits the configured budgets.
func (q *BoundedQueue) Resume(cp Checkpoint) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if cp.QueuedBytes > q.maxBytes || cp.QueuedItems > q.maxItems {
		return clock.ErrCheckpoint
	}
	var sum int
	for _, d := range cp.Pending {
		sum += d.Size
	}
	if sum != cp.QueuedBytes || len(cp.Pending) != cp.QueuedItems {
		return clock.ErrCheckpoint
	}
	return nil
}
