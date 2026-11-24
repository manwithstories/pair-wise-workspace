// Package stream provides the bounded queue connecting read, map and write
// stages. A slow consumer blocks the producer (back-pressure); cancellation or
// a terminal write failure propagates to every stage and releases resources.
package stream

import (
	"context"
	"sync"
	"sync/atomic"

	"mediadeliver/internal/model"
)

// PacketSink is the minimal contract the pipeline stages share.
type PacketSink interface {
	// Push blocks while the queue is full. It returns KindCancelled when ctx
	// is done and KindIO/KindCorruptMedia once the queue has failed.
	Push(ctx context.Context, p *model.Packet) error
	// Pop blocks until a packet, end-of-stream or an error is available.
	Pop(ctx context.Context) (*model.Packet, error)
	// FailWith propagates a terminal stage error to all blocked producers
	// and consumers. Subsequent calls keep the first error.
	FailWith(err error)
	// Close signals normal end-of-stream; buffered packets are drained first.
	Close()
	// Stats returns cumulative counters.
	Stats() Stats
}

// Stats exposes observability counters for the demo output.
type Stats struct {
	Enqueued            int64
	Dequeued            int64
	RejectedOversize    int64
	BackpressureEvents  int64 // times a producer found the queue full
	PeakBufferedPackets int64
	PeakBufferBytes     int64
}

// Queue is a fixed-capacity, context-aware bounded packet queue.
type Queue struct {
	capacity int
	maxBytes int // per-packet payload cap; oversized packets are rejected

	mu       sync.Mutex
	buf      []*model.Packet
	bufBytes int64
	closed   bool
	failed   error
	notFull  *sync.Cond
	notEmpty *sync.Cond

	stats Stats
}

// NewQueue creates a queue holding at most capacity packets; each packet
// payload must not exceed maxPacketBytes (model.KindLimitExceeded otherwise).
func NewQueue(capacity, maxPacketBytes int) *Queue {
	if capacity <= 0 {
		capacity = 1
	}
	q := &Queue{capacity: capacity, maxBytes: maxPacketBytes}
	q.notFull = sync.NewCond(&q.mu)
	q.notEmpty = sync.NewCond(&q.mu)
	return q
}

// Push enqueues p, blocking when full. It honors ctx cancellation.
func (q *Queue) Push(ctx context.Context, p *model.Packet) error {
	if p == nil {
		return model.New(model.KindInvalidInput, "stream", "push", "nil packet")
	}
	if len(p.Payload) > q.maxBytes {
		atomic.AddInt64(&q.stats.RejectedOversize, 1)
		return model.At(model.KindLimitExceeded, "stream", "push", p.Seq,
			"packet payload exceeds configured single-packet limit")
	}

	// Register cancellation wake-up outside the main lock to avoid races.
	stop := q.watchCancel(ctx)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()

	wasFull := false
	for len(q.buf) >= q.capacity && q.failed == nil && !q.closed {
		if !wasFull {
			atomic.AddInt64(&q.stats.BackpressureEvents, 1)
			wasFull = true
		}
		// Re-check ctx while holding the lock via the cond wait.
		if err := ctx.Err(); err != nil {
			return model.Wrap(model.KindCancelled, "stream", "push", p.Seq,
				"producer cancelled while queue full", err)
		}
		q.notFull.Wait()
	}
	if q.failed != nil {
		return q.failed
	}
	if q.closed {
		return model.New(model.KindIO, "stream", "push", "push after close")
	}
	if err := ctx.Err(); err != nil {
		return model.Wrap(model.KindCancelled, "stream", "push", p.Seq,
			"producer cancelled", err)
	}

	q.buf = append(q.buf, p)
	q.bufBytes += int64(len(p.Payload))
	atomic.AddInt64(&q.stats.Enqueued, 1)
	if n := int64(len(q.buf)); n > atomic.LoadInt64(&q.stats.PeakBufferedPackets) {
		atomic.StoreInt64(&q.stats.PeakBufferedPackets, n)
	}
	if q.bufBytes > atomic.LoadInt64(&q.stats.PeakBufferBytes) {
		atomic.StoreInt64(&q.stats.PeakBufferBytes, q.bufBytes)
	}
	q.notEmpty.Signal()
	return nil
}

// Pop removes and returns the next packet, blocking while empty.
func (q *Queue) Pop(ctx context.Context) (*model.Packet, error) {
	stop := q.watchCancel(ctx)
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.buf) == 0 && !q.closed && q.failed == nil {
		if err := ctx.Err(); err != nil {
			return nil, model.Wrap(model.KindCancelled, "stream", "pop", -1,
				"consumer cancelled while queue empty", err)
		}
		q.notEmpty.Wait()
	}
	if q.failed != nil {
		return nil, q.failed
	}
	if len(q.buf) == 0 {
		return nil, nil // normal end-of-stream
	}
	p := q.buf[0]
	q.buf = q.buf[1:]
	q.bufBytes -= int64(len(p.Payload))
	atomic.AddInt64(&q.stats.Dequeued, 1)
	q.notFull.Signal()
	return p, nil
}

// FailWith records the first terminal error and wakes every waiter.
func (q *Queue) FailWith(err error) {
	if err == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failed == nil {
		q.failed = err
	}
	q.notFull.Broadcast()
	q.notEmpty.Broadcast()
}

// Close marks end-of-stream; consumers drain remaining packets first.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
}

// Stats returns a consistent snapshot of the counters.
func (q *Queue) Stats() Stats {
	return Stats{
		Enqueued:            atomic.LoadInt64(&q.stats.Enqueued),
		Dequeued:            atomic.LoadInt64(&q.stats.Dequeued),
		RejectedOversize:    atomic.LoadInt64(&q.stats.RejectedOversize),
		BackpressureEvents:  atomic.LoadInt64(&q.stats.BackpressureEvents),
		PeakBufferedPackets: atomic.LoadInt64(&q.stats.PeakBufferedPackets),
		PeakBufferBytes:     atomic.LoadInt64(&q.stats.PeakBufferBytes),
	}
}

// watchCancel spawns a goroutine that broadcasts the conds when ctx ends, so a
// blocked Push/Pop wakes immediately. The returned stop releases the goroutine.
func (q *Queue) watchCancel(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.notFull.Broadcast()
			q.notEmpty.Broadcast()
			q.mu.Unlock()
		case <-done:
		}
	}()
	return func() { close(done) }
}
