// Package pipeline provides the bounded, back-pressured streaming core that
// connects ingest to the analysis/packaging stages. Queue depth never exceeds
// the configured capacity; a slow consumer blocks the upstream reader rather
// than buffering without limit. Cancellation retains only the last fully
// committed segment boundary so a restart resumes from a verified point.
package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Frame is one unit of work flowing through the pipeline. Kind discriminates
// samples, events and control markers.
type Frame struct {
	Kind     FrameKind
	TrackID  uint32
	Index    int
	TimeNS   int64
	DurNS    int64
	Keyframe bool
	Payload  []byte
	Meta     map[string]string
}

// FrameKind enumerates stream frame types.
type FrameKind uint8

const (
	FrameSample FrameKind = iota
	FrameEvent
	FrameSegmentCommit
	FrameCheckpoint
)

// Stats exposes queue telemetry for the quality report. All values are read
// under the queue lock and safe for concurrent access.
type Stats struct {
	Enqueued   uint64
	Dequeued   uint64
	Dropped    uint64
	PeakDepth  int
	BlockedNS  int64 // total time producers waited on a full queue
}

// ErrQueueClosed is returned after Close drains the pipeline.
var ErrQueueClosed = errors.New("pipeline: queue closed")

// Queue is a capacity-bounded blocking queue. A Send on a full queue blocks
// until a Receive frees a slot or ctx ends; this is the backpressure contract.
type Queue struct {
	capacity int
	ch       chan Frame
	mu       sync.Mutex
	stats    Stats
	closed   bool
}

// NewQueue creates a queue with the given positive capacity.
func NewQueue(capacity int) *Queue {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue{capacity: capacity, ch: make(chan Frame, capacity)}
}

// Capacity returns the configured bound.
func (q *Queue) Capacity() int { return q.capacity }

// Send enqueues a frame, blocking while full. On ctx cancellation it returns
// the context error without enqueuing, and records the blocked interval.
func (q *Queue) Send(ctx context.Context, f Frame) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrQueueClosed
	}
	depth := len(q.ch)
	if depth > q.stats.PeakDepth {
		q.stats.PeakDepth = depth
	}
	q.mu.Unlock()

	start := time.Now()
	select {
	case q.ch <- f:
		q.recordBlocked(time.Since(start))
		q.mu.Lock()
		q.stats.Enqueued++
		if d := len(q.ch); d > q.stats.PeakDepth {
			q.stats.PeakDepth = d
		}
		q.mu.Unlock()
		return nil
	case <-ctx.Done():
		q.recordBlocked(time.Since(start))
		return ctx.Err()
	}
}

// TrySend enqueues only if a slot is immediately free; otherwise it reports
// false so callers can apply explicit drop/backoff policy.
func (q *Queue) TrySend(f Frame) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	select {
	case q.ch <- f:
		q.stats.Enqueued++
		if d := len(q.ch); d > q.stats.PeakDepth {
			q.stats.PeakDepth = d
		}
		return true
	default:
		q.stats.Dropped++
		return false
	}
}

// Receive blocks until a frame is available, ctx ends, or the queue closes.
// After Close, a fully drained queue yields ErrQueueClosed.
func (q *Queue) Receive(ctx context.Context) (Frame, error) {
	select {
	case f, ok := <-q.ch:
		if !ok {
			return Frame{}, ErrQueueClosed
		}
		q.mu.Lock()
		q.stats.Dequeued++
		q.mu.Unlock()
		return f, nil
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// Close prevents further Sends and releases receivers after drain.
func (q *Queue) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
	q.mu.Unlock()
}

// Snapshot returns a copy of the telemetry counters.
func (q *Queue) Snapshot() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

func (q *Queue) recordBlocked(d time.Duration) {
	q.mu.Lock()
	q.stats.BlockedNS += d.Nanoseconds()
	q.mu.Unlock()
}
