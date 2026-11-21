// Package stream carries normalized media segments between pipeline stages
// through a bounded queue. Memory is capped by a byte budget, not by a block
// count alone, and a slow consumer causes backpressure rather than loss.
package stream

import (
	"context"
	"errors"
	"sync"

	"github.com/pion/rtp"

	"mediamerge/internal/fault"
)

// Block is one bounded chunk of media on the wire. Payload is an RTP packet
// so sequence, timestamp and SSRC travel with the bytes.
type Block struct {
	// Segment is the 1-based output segment number for digest reporting.
	Segment int
	// TrackID identifies the source track.
	TrackID int
	// Offset is the byte offset within the track's output stream.
	Offset int64
	// Packet carries sequence, media timestamp and payload.
	Packet *rtp.Packet
	// Digest is the SHA-256 of the payload, computed at enqueue time so
	// the consumer does not need to retain data for hashing.
	Digest [32]byte
}

// ByteSize is the accounted memory for the block: payload plus packet
// overhead. It excludes fields shared with the queue itself.
func (b *Block) ByteSize() int64 {
	if b == nil || b.Packet == nil {
		return 0
	}
	return int64(len(b.Packet.Payload)) + 64
}

// ErrClosed is returned when enqueueing to a closed queue.
var ErrClosed = errors.New("stream: queue closed")

// Queue is a fixed byte-budget FIFO with blocking producers.
type Queue struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond

	items  []*Block
	bytes  int64
	budget int64
	closed bool
	// highWater is the maximum observed occupancy, for the memory report.
	highWater int64
	// blocked counts producer waits, proving backpressure occurred.
	blocked int64
}

// NewQueue creates a queue with a payload budget in bytes.
func NewQueue(budgetBytes int64) (*Queue, error) {
	if budgetBytes <= 0 {
		return nil, fault.New(fault.StageStream, fault.CodeInvalidArgument,
			"queue byte budget must be positive")
	}
	q := &Queue{budget: budgetBytes}
	q.notEmpty = sync.NewCond(&q.mu)
	q.notFull = sync.NewCond(&q.mu)
	return q, nil
}

// Push enqueues b, blocking while the budget is exhausted. It returns an
// error when ctx is cancelled or the queue is closed, and it never drops a
// block that was accepted.
func (q *Queue) Push(ctx context.Context, b *Block) error {
	if b == nil {
		return fault.New(fault.StageStream, fault.CodeInvalidArgument, "nil block")
	}
	size := b.ByteSize()
	if size > q.budget {
		return fault.New(fault.StageStream, fault.CodeQueueFull,
			"block is larger than the whole queue budget; raise --chunk-bytes")
	}

	// Cancellation must wake a producer parked on notFull.
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.notFull.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	waited := false
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			if waited {
				q.blocked++
			}
			return fault.Wrap(fault.StageStream, fault.CodeCanceled, err, "producer cancelled while waiting for space")
		}
		if q.closed {
			return fault.New(fault.StageStream, fault.CodeWriteFailed, ErrClosed.Error())
		}
		if q.bytes+size <= q.budget {
			break
		}
		if !waited {
			q.blocked++
			waited = true
		}
		q.notFull.Wait()
	}
	q.items = append(q.items, b)
	q.bytes += size
	if q.bytes > q.highWater {
		q.highWater = q.bytes
	}
	q.notEmpty.Broadcast()
	return nil
}

// Pop removes and returns the oldest block, blocking until one arrives.
// It returns nil when the queue is drained and closed.
func (q *Queue) Pop(ctx context.Context) (*Block, error) {
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.notEmpty.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fault.Wrap(fault.StageStream, fault.CodeCanceled, err, "consumer cancelled")
		}
		if len(q.items) > 0 {
			b := q.items[0]
			q.items[0] = nil
			q.items = q.items[1:]
			q.bytes -= b.ByteSize()
			q.notFull.Broadcast()
			return b, nil
		}
		if q.closed {
			return nil, nil
		}
		q.notEmpty.Wait()
	}
}

// Close wakes all producers and consumers. Blocks already queued stay
// available to Pop so a clean drain can still process them.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
	q.mu.Unlock()
}

// Len returns the number of queued blocks.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Stats is a point-in-time queue report.
type Stats struct {
	// Bytes is the current accounted payload.
	Bytes int64
	// Budget is the configured cap.
	Budget int64
	// HighWater is the largest occupancy seen.
	HighWater int64
	// Blocked counts producer waits; non-zero proves backpressure fired.
	Blocked int64
	// Items is the current depth.
	Items int
}

func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{Bytes: q.bytes, Budget: q.budget, HighWater: q.highWater, Blocked: q.blocked, Items: len(q.items)}
}

// Closed reports whether Close has been called.
func (q *Queue) Closed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}
