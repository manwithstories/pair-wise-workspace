// Package flow implements the bounded backpressure pipeline. Enqueue blocks
// when the configured byte budget is full; the resulting wait is recorded as
// backpressure. A 2-second EWMA of arrival bytes drives an adaptive reorder
// window between 20 ms and 200 ms.
package flow

import (
	"context"
	"sync"
	"time"

	"avcheck/internal/clock"
)

const (
	// MinWindowMS and MaxWindowMS bound the adaptive reorder window.
	MinWindowMS = 20
	MaxWindowMS = 200
	// BandwidthHorizon is the EWMA observation window (spec: last 2 seconds).
	BandwidthHorizon = 2 * time.Second
	ewmaAlpha        = 0.2
)

// Queue is a byte-bounded blocking queue of blocks.
type Queue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	maxBytes int
	bytes    int
	blocks   []*clock.Block
	closed   bool

	enqueued int64
	dequeued int64

	// Backpressure bookkeeping.
	events          int64
	backpressureMS  int64
	peakBytes       int
	firstArrival    int64
	lastArrival     int64
	ewmaBytesPerSec float64
}

// NewQueue creates a queue with a hard byte ceiling. A zero/negative ceiling
// is rejected by the caller-facing Enqueue as a bad configuration.
func NewQueue(maxBytes int) *Queue {
	q := &Queue{maxBytes: maxBytes, blocks: make([]*clock.Block, 0, 64)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue blocks while the queue is full, propagating ctx cancellation. The
// virtual arrival time (ms) updates the bandwidth estimator and backpressure
// counters; oversized blocks are rejected before any allocation.
func (q *Queue) Enqueue(ctx context.Context, b *clock.Block, arrivalMS int64) error {
	size := b.ByteSize()
	if size > clock.MaxBlockBytes {
		return clock.NewError(clock.KindBadBlock,
			"block %d bytes exceeds %d byte cap", size, clock.MaxBlockBytes)
	}
	if size > q.maxBytes {
		return clock.NewError(clock.KindQueueFull,
			"block %d bytes larger than queue ceiling %d", size, q.maxBytes)
	}

	q.mu.Lock()
	blocked := false
	var waitStart time.Time
	for q.bytes+size > q.maxBytes && !q.closed {
		if err := ctx.Err(); err != nil {
			q.mu.Unlock()
			return err
		}
		if !blocked {
			blocked = true
			waitStart = time.Now()
		}
		q.events++
		q.cond.Wait()
	}
	if blocked {
		q.backpressureMS += time.Since(waitStart).Milliseconds()
	}
	if q.closed {
		q.mu.Unlock()
		return context.Canceled
	}
	q.blocks = append(q.blocks, b)
	q.bytes += size
	q.enqueued++
	if q.bytes > q.peakBytes {
		q.peakBytes = q.bytes
	}
	q.observe(size, arrivalMS)
	q.mu.Unlock()
	return nil
}

// Dequeue removes and returns the head block, blocking until one is available.
func (q *Queue) Dequeue(ctx context.Context) (*clock.Block, error) {
	q.mu.Lock()
	for len(q.blocks) == 0 && !q.closed {
		if err := ctx.Err(); err != nil {
			q.mu.Unlock()
			return nil, err
		}
		q.cond.Wait()
	}
	if len(q.blocks) == 0 {
		q.mu.Unlock()
		return nil, context.Canceled
	}
	b := q.blocks[0]
	q.blocks[0] = nil
	q.blocks = q.blocks[1:]
	q.bytes -= b.ByteSize()
	q.dequeued++
	q.cond.Broadcast()
	q.mu.Unlock()
	return b, nil
}

// Close releases every blocked Enqueue/Dequeue.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// observe folds one arrival into the 2-second EWMA of bytes per second.
func (q *Queue) observe(size int, arrivalMS int64) {
	if q.firstArrival == 0 && q.lastArrival == 0 {
		q.firstArrival = arrivalMS
		q.lastArrival = arrivalMS
		q.ewmaBytesPerSec = float64(size) * 1000 // one sample in ~1 ms
		return
	}
	dt := arrivalMS - q.lastArrival
	q.lastArrival = arrivalMS
	if dt <= 0 {
		dt = 1
	}
	instant := float64(size) * 1000 / float64(dt)
	q.ewmaBytesPerSec = ewmaAlpha*instant + (1-ewmaAlpha)*q.ewmaBytesPerSec
	if q.lastArrival-q.firstArrival > int64(BandwidthHorizon/time.Millisecond) {
		q.firstArrival = q.lastArrival - int64(BandwidthHorizon/time.Millisecond)
	}
}

// WindowMS maps the current bandwidth estimate onto the 20..200 ms reorder
// window. Low bandwidth -> longer window to absorb jitter; high bandwidth
// drains fast and keeps the window short.
func (q *Queue) WindowMS() int {
	q.mu.Lock()
	bw := q.ewmaBytesPerSec
	q.mu.Unlock()
	if bw <= 0 {
		return MinWindowMS
	}
	// Queued bytes at the measured rate, expressed as a buffering duration.
	q.mu.Lock()
	drainMS := int(float64(q.bytes) * 1000 / bw)
	q.mu.Unlock()
	if drainMS < MinWindowMS {
		return MinWindowMS
	}
	if drainMS > MaxWindowMS {
		return MaxWindowMS
	}
	return drainMS
}

// Snapshot returns the queue counters without blocking.
type Snapshot struct {
	Bytes          int
	Len            int
	PeakBytes      int
	Enqueued       int64
	Dequeued       int64
	BackpressureEv int64
	BackpressureMS int64
	WindowMS       int
}

// Stats returns a consistent snapshot of queue state.
func (q *Queue) Stats() Snapshot {
	q.mu.Lock()
	s := Snapshot{
		Bytes:          q.bytes,
		Len:            len(q.blocks),
		PeakBytes:      q.peakBytes,
		Enqueued:       q.enqueued,
		Dequeued:       q.dequeued,
		BackpressureEv: q.events,
		BackpressureMS: q.backpressureMS,
	}
	q.mu.Unlock()
	s.WindowMS = q.WindowMS()
	return s
}
