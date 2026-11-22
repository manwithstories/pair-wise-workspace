// Package pipeline connects ingest, metadata/mapping and remux with bounded
// queues so a slow downstream stage applies back-pressure all the way to the
// parser. Memory is bounded by queue depth times block size; a full queue
// blocks the producer instead of growing without limit.
package pipeline

import (
	"errors"

	"mediarun/internal/ingest"
)

// DefaultQueueDepth is the documented default (spec: 8).
const DefaultQueueDepth = 8

// blockMaxChunks bounds how many samples one block may contain; together with
// ingest.MaxBlockBytes this fixes the worst-case in-flight memory.
const blockMaxChunks = 64

// Block is one bounded unit of work moving through the stages.
type Block struct {
	Samples []*ingest.SampleEvent
}

// BoundedQueue is a capacity-capped channel wrapper. Send blocks on a full
// queue until a receiver makes room; Close unblocks producers and receivers.
type BoundedQueue struct {
	ch     chan *Block
	closed bool
}

// NewBoundedQueue creates a queue holding at most depth blocks.
func NewBoundedQueue(depth int) *BoundedQueue {
	if depth <= 0 {
		depth = DefaultQueueDepth
	}
	return &BoundedQueue{ch: make(chan *Block, depth)}
}

// ErrQueueClosed is returned by Send after the queue is shut down.
var ErrQueueClosed = errors.New("pipeline: queue closed")

// Push blocks while the queue is full, propagating back-pressure. After Close
// it returns ErrQueueClosed so a producer can unwind cleanly on cancellation.
func (q *BoundedQueue) Push(b *Block) error {
	if q.closed {
		return ErrQueueClosed
	}
	q.ch <- b
	return nil
}

// Pop blocks until a block is available. The boolean is false once the queue
// is drained and closed, mirroring a channel's ok value.
func (q *BoundedQueue) Pop() (*Block, bool) {
	b, ok := <-q.ch
	return b, ok
}

// Close marks the queue shut and closes the channel. It is idempotent.
func (q *BoundedQueue) Close() {
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

// Len reports current occupancy, mainly for diagnostics.
func (q *BoundedQueue) Len() int { return len(q.ch) }

// Cap reports the configured capacity.
func (q *BoundedQueue) Cap() int { return cap(q.ch) }

// chunkBlock groups streamed samples into fixed-size blocks, never exceeding
// either the chunk count ceiling or the 4 MiB byte ceiling per block.
type chunkBlock struct {
	current *Block
	bytes   int64
}

func newChunkBlock() *chunkBlock {
	return &chunkBlock{current: &Block{}}
}

// add appends a sample and reports true when the block is full and must be
// flushed (the caller then starts a fresh block).
func (c *chunkBlock) add(ev *ingest.SampleEvent) bool {
	c.current.Samples = append(c.current.Samples, ev)
	c.bytes += ev.Size
	fullByCount := len(c.current.Samples) >= blockMaxChunks
	fullByBytes := c.bytes >= ingest.MaxBlockBytes
	return fullByCount || fullByBytes
}

// reset starts a new empty block after a flush.
func (c *chunkBlock) reset() {
	c.current = &Block{}
	c.bytes = 0
}
