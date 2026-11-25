// Package pipeline provides bounded queues, memory accounting and cancellation.
package pipeline

import (
	"context"
	"sync"
	"time"

	"mediaproc/internal/model"
	"mediaproc/internal/segment"
)

// Item is one verified-or-pending work unit.
type Item struct {
	ID      string
	Offset  int64
	Size    int64
	Segment *segment.Segment
}

// Metrics records queue high-water marks and cleanup counts.
type Metrics struct {
	PeakQueue    int   `json:"peak_queue"`
	PeakMemory   int64 `json:"peak_memory"`
	Published    int   `json:"published"`
	Cancelled    int   `json:"cancelled"`
	Backpressure int   `json:"backpressure_waits"`
}

// Runner applies count, byte-memory and cancellation bounds to a work queue.
type Runner struct {
	QueueCapacity int
	MaxMemory     int64
	SlowConsumer  time.Duration
	Cleanup       func(Item)
	// StartGate, when non-nil, blocks consumption until Resume is called.
	// This lets tests/demos deterministically fill the bounded queue.
	StartGate chan struct{}

	queue chan Item

	mu           sync.Mutex
	used         int64
	metrics      Metrics
	closed       bool
	cancelOffset int64
	resumeOnce   sync.Once
}

// NewRunner constructs a bounded runner.
func NewRunner(queueCapacity int, maxMemory int64, slow time.Duration) *Runner {
	if queueCapacity <= 0 {
		queueCapacity = 1
	}
	if maxMemory <= 0 {
		maxMemory = 8 << 20
	}
	return &Runner{
		QueueCapacity: queueCapacity,
		MaxMemory:     maxMemory,
		SlowConsumer:  slow,
		queue:         make(chan Item, queueCapacity),
	}
}

// Resume closes StartGate exactly once, allowing the consumer loop to start.
func (r *Runner) Resume() {
	r.resumeOnce.Do(func() {
		if r.StartGate != nil {
			close(r.StartGate)
		}
	})
}

// Metrics returns a snapshot of queue metrics.
func (r *Runner) Metrics() Metrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.metrics
	m.PeakQueue = max(m.PeakQueue, len(r.queue))
	return m
}

// Submit applies memory and queue backpressure. On cancellation while blocked
// it returns a structured pipeline fault carrying the item's source offset.
func (r *Runner) Submit(ctx context.Context, item Item) error {
	if item.Size <= 0 && item.Segment != nil {
		item.Size = EstimateSize(item.Segment)
	}
	if item.Size > r.MaxMemory {
		return model.NewError(model.PhasePipeline, item.Offset, false,
			"queue item exceeds memory budget", nil)
	}

	waited := false
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return model.NewError(model.PhasePipeline, item.Offset, true, "queue closed", nil)
		}
		if r.used+item.Size <= r.MaxMemory {
			r.used += item.Size
			if r.used > r.metrics.PeakMemory {
				r.metrics.PeakMemory = r.used
			}
			r.mu.Unlock()
			break
		}
		r.metrics.Backpressure++
		waited = true
		r.mu.Unlock()
		if !sleepWithContext(ctx, time.Millisecond) {
			return model.NewError(model.PhasePipeline, item.Offset, true,
				"cancelled while waiting for memory backpressure", ctx.Err())
		}
	}
	_ = waited

	select {
	case r.queue <- item:
		r.mu.Lock()
		if n := len(r.queue); n > r.metrics.PeakQueue {
			r.metrics.PeakQueue = n
		}
		r.mu.Unlock()
		return nil
	case <-ctx.Done():
		r.release(item.Size)
		r.mu.Lock()
		r.cancelOffset = item.Offset
		r.mu.Unlock()
		return model.NewError(model.PhasePipeline, item.Offset, true,
			"cancelled while queue was full", ctx.Err())
	}
}

// Run consumes queued items until ctx is canceled or Close is called. A slow
// consumer delay is applied through context-aware sleep.
func (r *Runner) Run(ctx context.Context, consume func(context.Context, Item) error) error {
	if r.StartGate != nil {
		select {
		case <-r.StartGate:
		case <-ctx.Done():
			return r.cancelCleanup(ctx)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return r.cancelCleanup(ctx)
		case item, ok := <-r.queue:
			if !ok {
				return nil
			}
			if r.SlowConsumer > 0 && !sleepWithContext(ctx, r.SlowConsumer) {
				r.cleanupUnpublished(item)
				return r.cancelCleanup(ctx)
			}
			if err := consume(ctx, item); err != nil {
				if ctx.Err() != nil {
					r.cleanupUnpublished(item)
					return r.cancelCleanup(ctx)
				}
				r.release(item.Size)
				return err
			}
			r.release(item.Size)
			r.mu.Lock()
			r.metrics.Published++
			r.mu.Unlock()
		}
	}
}

// Close forbids further submissions and closes the channel.
func (r *Runner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
}

func (r *Runner) release(size int64) {
	r.mu.Lock()
	r.used -= size
	if r.used < 0 {
		r.used = 0
	}
	r.mu.Unlock()
}

func (r *Runner) cleanupUnpublished(item Item) {
	r.release(item.Size)
	r.mu.Lock()
	r.metrics.Cancelled++
	r.mu.Unlock()
	if r.Cleanup != nil {
		r.Cleanup(item)
	}
}

func (r *Runner) cancelCleanup(ctx context.Context) error {
	var pending []Item
	for {
		select {
		case item := <-r.queue:
			pending = append(pending, item)
		default:
			for _, item := range pending {
				r.release(item.Size)
				r.mu.Lock()
				r.metrics.Cancelled++
				r.mu.Unlock()
				if r.Cleanup != nil {
					r.Cleanup(item)
				}
			}
			r.mu.Lock()
			offset := r.cancelOffset
			if r.used < 0 {
				r.used = 0
			}
			r.mu.Unlock()
			return model.NewError(model.PhasePipeline, offset, true,
				"pipeline canceled; unpublished segments cleaned", ctx.Err())
		}
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// EstimateSize accounts for the retained block payloads plus JSON metadata.
func EstimateSize(s *segment.Segment) int64 {
	if s == nil {
		return 0
	}
	var n int64 = 512
	for _, b := range s.Blocks {
		if b == nil {
			continue
		}
		n += 128
		for _, f := range b.Frames {
			n += int64(len(f))
		}
	}
	return n
}
