// Package stream provides memory-bounded pipeline queues with cancellation and
// explicit processed-position bookkeeping.
package stream

import (
	"context"
	"fmt"
	"io"
	"sync"

	"mediacheck/internal/media"
)

// Item is one bounded-queue work unit.
type Item struct {
	Offset  int64
	Length  int
	PTS     int64
	Payload []byte
}

// EndOffset returns the exclusive byte position covered by the item.
func (i Item) EndOffset() int64 { return i.Offset + int64(i.Length) }

// Config configures queue count and byte memory limits.
type Config struct {
	MaxItems int
	MaxBytes int
}

// Queue is a backpressured FIFO. Byte accounting is based on Item.Payload.
type Queue struct {
	mu        sync.Mutex
	cond      *sync.Cond
	maxItems  int
	maxBytes  int
	items     []Item
	byteCount int
	highWater int
	processed int64
	closed    bool
}

// NewQueue creates a queue. Zero limits fall back to safe small defaults.
func NewQueue(cfg Config) *Queue {
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = 64
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 1 << 20
	}
	q := &Queue{maxItems: cfg.MaxItems, maxBytes: cfg.MaxBytes}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Put enqueues an item, applying both count and byte backpressure.
func (q *Queue) Put(ctx context.Context, item Item) error {
	if len(item.Payload) > q.maxBytes {
		return media.E(media.StageQueue, media.KindResource, item.Offset,
			fmt.Errorf("item size %d exceeds byte budget %d", len(item.Payload), q.maxBytes))
	}
	stopWatch := q.watchContext(ctx)
	defer stopWatch()

	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return media.E(media.StageQueue, media.KindCanceled, item.Offset, err).WithRetryable()
		}
		if q.closed {
			return io.ErrClosedPipe
		}
		if len(q.items) < q.maxItems && q.byteCount+len(item.Payload) <= q.maxBytes {
			q.items = append(q.items, item)
			q.byteCount += len(item.Payload)
			if q.byteCount > q.highWater {
				q.highWater = q.byteCount
			}
			q.cond.Broadcast()
			return nil
		}
		q.cond.Wait()
	}
}

// Get dequeues one item. It blocks while empty and open. At clean end it
// returns io.EOF after all queued work has been drained.
func (q *Queue) Get(ctx context.Context) (Item, error) {
	stopWatch := q.watchContext(ctx)
	defer stopWatch()

	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return Item{}, media.E(media.StageQueue, media.KindCanceled, q.processed, err).
				WithRetryable()
		}
		if len(q.items) > 0 {
			item := q.items[0]
			q.items[0] = Item{}
			q.items = q.items[1:]
			q.byteCount -= len(item.Payload)
			q.cond.Broadcast()
			return item, nil
		}
		if q.closed {
			return Item{}, io.EOF
		}
		q.cond.Wait()
	}
}

// Ack records the durable processed byte position.
func (q *Queue) Ack(position int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if position > q.processed {
		q.processed = position
	}
}

// ProcessedPosition returns the highest acknowledged byte position.
func (q *Queue) ProcessedPosition() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.processed
}

// Close marks the queue complete. Queued items may still be drained.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Len returns queued item count.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// ByteLen returns queued payload bytes.
func (q *Queue) ByteLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.byteCount
}

// HighWaterBytes returns the maximum observed queued payload bytes.
func (q *Queue) HighWaterBytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.highWater
}

// MaxBytes returns the configured byte ceiling.
func (q *Queue) MaxBytes() int { return q.maxBytes }

func (q *Queue) watchContext(ctx context.Context) func() {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.cond.Broadcast()
			q.mu.Unlock()
		case <-done:
		}
	}()
	return func() { close(done) }
}
