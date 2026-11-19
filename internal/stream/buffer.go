// Package stream provides the bounded hand-off queue between producers and
// consumers. Two independent limits (bytes and media duration) bound memory;
// slow consumers trigger an explicit, configurable policy instead of
// unbounded backlog.
package stream

import (
	"sync"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

// Policy selects what a producer does when the queue is full.
type Policy string

const (
	// Block waits until space is available, the queue closes, or the
	// context is canceled.
	Block Policy = "block"
	// Drop rejects the incoming item immediately with CodeQueueLimit.
	Drop Policy = "drop"
	// DropOldest evicts the oldest queued item to make room and reports it.
	DropOldest Policy = "drop-oldest"
)

// ParsePolicy resolves a policy name.
func ParsePolicy(s string) (Policy, error) {
	switch Policy(s) {
	case Block, Drop, DropOldest:
		return Policy(s), nil
	}
	return "", fault.New(fault.CodeInvalidArgument, "stream.policy",
		"unknown policy %q (block/drop/drop-oldest)", s)
}

// Item is a queued unit with an explicit byte and duration charge.
type Item struct {
	Seq   int64
	Bytes int64
	Span  clock.Span
	Label string
	Value any
}

// Stats is a snapshot of queue activity.
type Stats struct {
	Enqueued  int64 `json:"enqueued"`
	Dequeued  int64 `json:"dequeued"`
	Dropped   int64 `json:"dropped"`
	Evicted   int64 `json:"evicted"`
	PeakBytes int64 `json:"peak_bytes"`
	PeakTicks int64 `json:"peak_ticks"`
	Waits     int64 `json:"waits"`
	Closed    bool  `json:"closed"`
}

// Queue is a bounded FIFO. It is safe for one or more producers and one or
// more consumers; Close wakes every waiter.
type Queue struct {
	mu       sync.Mutex
	notFull  *sync.Cond
	notEmpty *sync.Cond

	maxBytes int64
	maxTicks int64
	items    []*Item

	bytes int64
	ticks int64
	seq   int64

	closed     bool
	closeCause error
	policy     Policy

	stats Stats
}

// Config sizes the queue. A zero limit disables that dimension; at least one
// must be positive.
type Config struct {
	MaxBytes int64
	MaxTicks int64
	Policy   Policy
}

// New builds a queue.
func New(cfg Config) (*Queue, error) {
	if cfg.MaxBytes <= 0 && cfg.MaxTicks <= 0 {
		return nil, fault.New(fault.CodeInvalidArgument, "stream.new",
			"at least one of MaxBytes/MaxTicks must be positive")
	}
	if cfg.Policy == "" {
		cfg.Policy = Block
	}
	if _, err := ParsePolicy(string(cfg.Policy)); err != nil {
		return nil, err
	}
	q := &Queue{maxBytes: cfg.MaxBytes, maxTicks: cfg.MaxTicks, policy: cfg.Policy}
	q.notFull = sync.NewCond(&q.mu)
	q.notEmpty = sync.NewCond(&q.mu)
	return q, nil
}

// Policy returns the configured full-queue policy.
func (q *Queue) Policy() Policy { return q.policy }

func (q *Queue) overLimits(addBytes, addTicks int64) bool {
	if q.maxBytes > 0 && q.bytes+addBytes > q.maxBytes {
		return true
	}
	if q.maxTicks > 0 && q.ticks+addTicks > q.maxTicks {
		return true
	}
	return false
}

// Push enqueues item, applying the full-queue policy. It returns the item
// evicted by DropOldest (nil otherwise).
func (q *Queue) Push(item Item) (*Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	addBytes := item.Bytes
	addTicks := int64(item.Span.Len())
	for {
		if q.closed {
			return nil, q.closedErrLocked("push")
		}
		if !q.overLimits(addBytes, addTicks) {
			break
		}
		switch q.policy {
		case Drop:
			q.stats.Dropped++
			return nil, fault.New(fault.CodeQueueLimit, "stream.push",
				"queue full: %d/%d bytes, %d/%d ticks", q.bytes, q.maxBytes, q.ticks, q.maxTicks).
				WithTrack(item.Label)
		case DropOldest:
			if len(q.items) == 0 {
				// A single item larger than the whole queue cannot be made
				// to fit by eviction; reject it rather than loop forever.
				q.stats.Dropped++
				return nil, fault.New(fault.CodeQueueLimit, "stream.push",
					"item of %d bytes exceeds queue capacity %d", addBytes, q.maxBytes).
					WithTrack(item.Label)
			}
			old := q.items[0]
			q.items = q.items[1:]
			q.bytes -= old.Bytes
			q.ticks -= int64(old.Span.Len())
			q.stats.Evicted++
			q.notFull.Broadcast()
			q.enqueueLocked(item)
			return old, nil
		default:
			q.stats.Waits++
			q.notFull.Wait()
		}
	}
	q.enqueueLocked(item)
	return nil, nil
}

func (q *Queue) enqueueLocked(item Item) {
	q.seq++
	item.Seq = q.seq
	q.items = append(q.items, &item)
	q.bytes += item.Bytes
	q.ticks += int64(item.Span.Len())
	q.stats.Enqueued++
	if q.bytes > q.stats.PeakBytes {
		q.stats.PeakBytes = q.bytes
	}
	if q.ticks > q.stats.PeakTicks {
		q.stats.PeakTicks = q.ticks
	}
	q.notEmpty.Signal()
}

// TryPush enqueues item only if it fits without waiting. ok is false when
// the item would exceed a limit; err is non-nil only when the queue is
// closed. It lets a producer emulate blocking with an explicit, deterministic
// consumer step instead of parking a goroutine.
func (q *Queue) TryPush(item Item) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false, q.closedErrLocked("trypush")
	}
	if q.overLimits(item.Bytes, int64(item.Span.Len())) {
		return false, nil
	}
	q.enqueueLocked(item)
	return true, nil
}

// Pop removes the oldest item. ok is false when the queue is empty and
// closed, so consumers can drain and then exit. Pop returns the zero Item and
// false while closed with items still queued only after those are drained.
func (q *Queue) Pop() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		if q.closed {
			return Item{}, false
		}
		q.notEmpty.Wait()
		if q.closed && len(q.items) == 0 {
			return Item{}, false
		}
	}
	it := q.items[0]
	q.items = q.items[1:]
	q.bytes -= it.Bytes
	q.ticks -= int64(it.Span.Len())
	q.stats.Dequeued++
	q.notFull.Signal()
	return *it, true
}

// Close marks the queue closed with a cause and wakes every waiter. Producers
// waiting for space get a CodeQueueClosed error; consumers drain remaining
// items and then see ok=false.
func (q *Queue) Close(cause error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if cause == nil {
		cause = fault.New(fault.CodeQueueClosed, "stream.close", "queue closed")
	}
	q.closed = true
	q.closeCause = cause
	q.stats.Closed = true
	q.notFull.Broadcast()
	q.notEmpty.Broadcast()
}

// Closed reports whether the queue has been closed.
func (q *Queue) Closed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// Cancel closes the queue because the caller's context was canceled.
func (q *Queue) Cancel(cause error) {
	q.Close(fault.Wrap(fault.CodeQueueCanceled, "stream.cancel", cause, "canceled"))
}

func (q *Queue) closedErrLocked(op string) error {
	cause := q.closeCause
	if cause == nil {
		cause = fault.New(fault.CodeQueueClosed, "stream."+op, "queue closed")
	}
	return fault.Wrap(fault.CodeQueueClosed, "stream."+op, cause, "woke up to a closed queue")
}

// Stats returns a snapshot.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.stats
	s.PeakBytes = q.stats.PeakBytes
	return s
}

// Len returns the number of queued items.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Bytes returns the queued byte charge.
func (q *Queue) Bytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

// Drain removes and returns all queued items. Used when a consumer exits
// early so buffered memory is released deterministically.
func (q *Queue) Drain() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Item, len(q.items))
	for i, it := range q.items {
		out[i] = *it
	}
	q.items = nil
	q.bytes = 0
	q.ticks = 0
	q.notFull.Broadcast()
	return out
}
