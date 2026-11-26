// Package pipeline runs the parse → transform → verify stages behind a
// byte-bounded queue: 64 KiB blocks with a hard 16 MiB ceiling. A slow
// consumer backpressures the producer instead of dropping data, and every
// segment is committed atomically at a whole-segment boundary.
package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"

	"aircheck/internal/media"
)

// Defaults fixed by the delivery contract.
const (
	DefaultBlockBytes     = 64 << 10 // 64 KiB
	DefaultMaxBufferBytes = 16 << 20 // 16 MiB
)

// ErrClosed is returned by Put/Get after the queue is closed.
var ErrClosed = errors.New("pipeline: queue closed")

// ErrCancelled is returned when the context is cancelled while blocked.
var ErrCancelled = errors.New("pipeline: cancelled")

// Block is one 64 KiB (or shorter, at a segment tail) slice of a segment.
type Block struct {
	Segment  int   // manifest index of the owning segment
	Seq      int64 // sequence number from the manifest
	Index    int   // block index within the segment
	Last     bool  // final block of the segment
	Data     []byte
	URI      string         // root-relative source path, for fault reports
	Duration media.Duration // declared segment duration
}

// Stats reports queue behaviour for the acceptance metrics.
type Stats struct {
	BlockBytes   int64
	MaxBytes     int64
	PeakBytes    int64
	Puts         int64
	Backpressure int64 // Puts that had to wait for room
	WaitMs       int64 // total time spent waiting for room
	MaxWaitMs    int64
}

// Queue is a bounded FIFO of byte slices. Both Put and Get block, so the
// pipeline naturally applies backpressure without losing blocks.
type Queue struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond

	blockBytes int64
	maxBytes   int64
	buf        []*Block
	bytes      int64
	closed     bool

	stats Stats
}

// NewQueue builds a queue with the given block and buffer limits.
func NewQueue(blockBytes, maxBytes int64) (*Queue, error) {
	if blockBytes <= 0 || maxBytes <= 0 {
		return nil, errors.New("pipeline: block and buffer limits must be positive")
	}
	if blockBytes > maxBytes {
		return nil, errors.New("pipeline: block size exceeds buffer limit")
	}
	q := &Queue{blockBytes: blockBytes, maxBytes: maxBytes}
	q.notEmpty = sync.NewCond(&q.mu)
	q.notFull = sync.NewCond(&q.mu)
	q.stats.BlockBytes = blockBytes
	q.stats.MaxBytes = maxBytes
	return q, nil
}

// BlockBytes reports the chunk size.
func (q *Queue) BlockBytes() int64 { return q.blockBytes }

// MaxBytes reports the buffer ceiling.
func (q *Queue) MaxBytes() int64 { return q.maxBytes }

// Put adds a block, waiting while the buffer is full. A block larger than the
// ceiling is rejected rather than silently accepted.
func (q *Queue) Put(ctx context.Context, b *Block) error {
	if int64(len(b.Data)) > q.maxBytes {
		return errors.New("pipeline: block larger than buffer ceiling")
	}
	start := time.Now()
	waited := false
	// Wake the condition variables when the context is cancelled: Cond.Wait
	// cannot select on a channel, so a watcher broadcasts instead.
	stop := context.AfterFunc(ctx, func() { q.broadcastAll() })
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.bytes+int64(len(b.Data)) > q.maxBytes && !q.closed {
		if ctx.Err() != nil {
			return ErrCancelled
		}
		if !waited {
			waited = true
			q.stats.Backpressure++
		}
		q.notFull.Wait()
	}
	if q.closed {
		return ErrClosed
	}
	q.buf = append(q.buf, b)
	q.bytes += int64(len(b.Data))
	if q.bytes > q.stats.PeakBytes {
		q.stats.PeakBytes = q.bytes
	}
	q.stats.Puts++
	if waited {
		w := time.Since(start).Milliseconds()
		q.stats.WaitMs += w
		if w > q.stats.MaxWaitMs {
			q.stats.MaxWaitMs = w
		}
	}
	q.notEmpty.Signal()
	return nil
}

// Get removes the oldest block, waiting while the queue is empty. It returns
// ErrClosed once the queue is drained and closed.
func (q *Queue) Get(ctx context.Context) (*Block, error) {
	stop := context.AfterFunc(ctx, func() { q.broadcastAll() })
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.buf) == 0 && !q.closed {
		if ctx.Err() != nil {
			return nil, ErrCancelled
		}
		q.notEmpty.Wait()
	}
	if len(q.buf) == 0 {
		return nil, ErrClosed
	}
	b := q.buf[0]
	q.buf = q.buf[1:]
	q.bytes -= int64(len(b.Data))
	q.notFull.Broadcast()
	return b, nil
}

// Close marks the queue closed; Get drains what is buffered first. Puts fail.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.broadcastAll()
}

// broadcastAll wakes both waits; callers must not hold q.mu.
func (q *Queue) broadcastAll() {
	q.mu.Lock()
	q.notEmpty.Broadcast()
	q.notFull.Broadcast()
	q.mu.Unlock()
}

// Stats returns a snapshot.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

// Split breaks data into Blocks tagged with the segment identity. The final
// block is marked Last; an empty segment yields no blocks.
func Split(segmentIndex int, seq int64, uri string, dur media.Duration, data []byte, blockBytes int64) []*Block {
	if len(data) == 0 {
		return nil
	}
	n := (len(data) + int(blockBytes) - 1) / int(blockBytes)
	out := make([]*Block, 0, n)
	for i := 0; i < n; i++ {
		lo := i * int(blockBytes)
		hi := lo + int(blockBytes)
		if hi > len(data) {
			hi = len(data)
		}
		out = append(out, &Block{
			Segment: segmentIndex, Seq: seq, Index: i,
			Last: i == n-1, Data: data[lo:hi], URI: uri, Duration: dur,
		})
	}
	return out
}
