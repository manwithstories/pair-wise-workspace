// Package stream provides the byte-bounded processing pipeline used between
// alignment and durable checkpoint/manifest output.
package stream

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/relayteam/relay/internal/media"
)

// ErrClosed is returned when a queue is used after Close.
var ErrClosed = errors.New("stream queue closed")

// Queue is a weighted, bounded queue. Its byte accounting covers items that
// are queued and items currently being handled, so a slow consumer produces
// backpressure instead of allowing an unbounded heap backlog.
type Queue[T any] struct {
	ch       chan envelope[T]
	maxBytes int64

	mu        sync.Mutex
	cond      *sync.Cond
	current   int64
	peak      int64
	waitCount int64
	waitNanos int64
	closed    bool
}

type envelope[T any] struct {
	item  T
	bytes int64
}

// Stats is a point-in-time backpressure snapshot.
type Stats struct {
	CurrentBytes int64 `json:"current_bytes"`
	PeakBytes    int64 `json:"peak_bytes"`
	MaxBytes     int64 `json:"max_bytes"`
	WaitCount    int64 `json:"wait_count"`
	WaitMillis   int64 `json:"wait_millis"`
}

// NewQueue creates a queue with at most slotCount buffered envelopes and
// maxBytes of weighted in-flight data.
func NewQueue[T any](slotCount int, maxBytes int64) (*Queue[T], error) {
	if maxBytes <= 0 {
		return nil, media.E(media.ErrArgument, "stream.new_queue", media.Meta{},
			errors.New("max-inflight-bytes must be positive"))
	}
	if slotCount <= 0 {
		slotCount = 1
	}
	q := &Queue[T]{ch: make(chan envelope[T], slotCount), maxBytes: maxBytes}
	q.cond = sync.NewCond(&q.mu)
	return q, nil
}

// MaxBytes returns the configured byte ceiling.
func (q *Queue[T]) MaxBytes() int64 { return q.maxBytes }

// Push reserves weight and enqueues item. It blocks under backpressure.
func (q *Queue[T]) Push(ctx context.Context, item T, weight int64) error {
	if weight <= 0 {
		weight = 1
	}
	if weight > q.maxBytes {
		return media.E(media.ErrArgument, "stream.push", media.Meta{},
			errors.New("item weight exceeds max-inflight-bytes"))
	}

	q.mu.Lock()
	wakeCancel := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
	defer wakeCancel()
	waited := false
	start := time.Now()
	for q.current+weight > q.maxBytes && !q.closed {
		if ctx.Err() != nil {
			q.mu.Unlock()
			return ctx.Err()
		}
		if !waited {
			q.waitCount++
			waited = true
		}
		q.cond.Wait()
	}
	if waited {
		q.waitNanos += time.Since(start).Nanoseconds()
	}
	if q.closed {
		q.mu.Unlock()
		return ErrClosed
	}
	q.current += weight
	if q.current > q.peak {
		q.peak = q.current
	}
	q.mu.Unlock()

	select {
	case q.ch <- envelope[T]{item: item, bytes: weight}:
		return nil
	case <-ctx.Done():
		q.Release(weight)
		return ctx.Err()
	}
}

// Pop returns the next item. The caller must invoke Release after handling.
func (q *Queue[T]) Pop(ctx context.Context) (T, int64, error) {
	select {
	case env, ok := <-q.ch:
		if !ok {
			var zero T
			return zero, 0, ErrClosed
		}
		return env.item, env.bytes, nil
	case <-ctx.Done():
		var zero T
		return zero, 0, ctx.Err()
	}
}

// Release returns weight previously obtained through Pop.
func (q *Queue[T]) Release(weight int64) {
	q.mu.Lock()
	q.current -= weight
	if q.current < 0 {
		q.current = 0
	}
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Close closes the channel and wakes blocked producers. Pending items can
// still be drained until the channel is empty.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.ch)
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

// Stats returns the current peak and wait metrics.
func (q *Queue[T]) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{
		CurrentBytes: q.current,
		PeakBytes:    q.peak,
		MaxBytes:     q.maxBytes,
		WaitCount:    q.waitCount,
		WaitMillis:   q.waitNanos / int64(time.Millisecond),
	}
}

// SlowHook sleeps for d on every nth handled item. n<=1 slows every item and
// n<=0 disables the hook. It returns false if context cancellation interrupted.
func SlowHook(ctx context.Context, handled int64, every int, d time.Duration) error {
	if every <= 0 || d <= 0 || handled%int64(every) != 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
