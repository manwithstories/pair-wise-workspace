// Package pipeline connects the parse, reorder, clock, audio, index and
// container stages into a single back-pressured, cancellable data flow.
//
// Memory is bounded two ways at the same time: a byte budget and a packet
// count. When either limit is reached the producer blocks; a slow consumer
// therefore applies back pressure all the way upstream instead of letting
// queues grow without bound. On cancellation every blocked stage is released.
package pipeline

import (
	"context"
	"sync/atomic"

	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/stream"
)

// Config bounds the pipeline and sets the slow-consumer deadline.
type Config struct {
	// MaxBufferBytes caps the summed raw datagram bytes across all queues.
	MaxBufferBytes int64
	// MaxBufferPackets caps the number of datagrams held at any moment.
	MaxBufferPackets int
	// ConsumerTimeoutNS bounds how long a stage may wait for a slow consumer
	// before the run is canceled with a timeout error. Zero disables it.
	ConsumerTimeoutNS int64
}

// Validate enforces positive bounds.
func (c Config) Validate() *mediaerr.Error {
	if c.MaxBufferBytes <= 0 {
		return mediaerr.New(mediaerr.StagePipeline, mediaerr.KindInvalidInput,
			"MaxBufferBytes must be positive")
	}
	if c.MaxBufferPackets <= 0 {
		return mediaerr.New(mediaerr.StagePipeline, mediaerr.KindInvalidInput,
			"MaxBufferPackets must be positive")
	}
	if c.ConsumerTimeoutNS < 0 {
		return mediaerr.New(mediaerr.StagePipeline, mediaerr.KindInvalidInput,
			"ConsumerTimeoutNS must be zero or positive")
	}
	return nil
}

// boundedQueue is a thread-safe context-aware FIFO bounded by both packet
// count (the buffered channel capacity) and summed raw bytes (an atomic
// counter checked before each send). A full or over-budget queue blocks the
// sender; canceling the context releases every blocked goroutine.
type boundedQueue struct {
	ch chan *stream.Packet
	// freed is signaled (never carrying data) whenever Receive removes a
	// packet, so senders blocked on the byte budget can retry without ever
	// stealing a packet destined for the real consumer.
	freed     chan struct{}
	bytes     atomic.Int64
	maxBytes  int64
	peakBytes atomic.Int64
}

func newBoundedQueue(maxBytes int64, maxPkts int) *boundedQueue {
	return &boundedQueue{
		ch:       make(chan *stream.Packet, maxPkts),
		freed:    make(chan struct{}, 1),
		maxBytes: maxBytes,
	}
}

// Send enqueues a packet, honoring the byte budget and cancellation.
func (q *boundedQueue) Send(ctx context.Context, p *stream.Packet) *mediaerr.Error {
	size := int64(len(p.Raw))
	for {
		if err := ctx.Err(); err != nil {
			return mediaerr.Wrap(mediaerr.StagePipeline, mediaerr.KindCanceled,
				"send interrupted by cancellation", err)
		}
		// The byte budget is a soft admission check; the packet cap is the
		// hard channel bound. Retrying after a receive keeps both enforced.
		if q.bytes.Load()+size > q.maxBytes {
			select {
			case <-ctx.Done():
				return mediaerr.Wrap(mediaerr.StagePipeline, mediaerr.KindCanceled,
					"byte-budget wait canceled", ctx.Err())
			case <-q.freed:
				continue
			}
		}
		select {
		case q.ch <- p:
			q.addBytes(size)
			return nil
		case <-ctx.Done():
			return mediaerr.Wrap(mediaerr.StagePipeline, mediaerr.KindCanceled,
				"backpressure wait canceled", ctx.Err())
		}
	}
}

// signalFreed wakes a byte-budget-blocked sender without blocking.
func (q *boundedQueue) signalFreed() {
	select {
	case q.freed <- struct{}{}:
	default:
	}
}

func (q *boundedQueue) addBytes(delta int64) {
	cur := q.bytes.Add(delta)
	for {
		peak := q.peakBytes.Load()
		if cur <= peak || q.peakBytes.CompareAndSwap(peak, cur) {
			return
		}
	}
}

// Receive removes the head packet. The boolean follows channel semantics: it
// is false once the channel is closed and fully drained.
func (q *boundedQueue) Receive(ctx context.Context) (*stream.Packet, bool, *mediaerr.Error) {
	select {
	case p, ok := <-q.ch:
		if !ok {
			return nil, false, nil
		}
		q.addBytes(-int64(len(p.Raw)))
		q.signalFreed()
		return p, true, nil
	case <-ctx.Done():
		return nil, false, mediaerr.Wrap(mediaerr.StagePipeline, mediaerr.KindCanceled,
			"receive wait canceled", ctx.Err())
	}
}

// Close signals end of stream; buffered packets remain readable.
func (q *boundedQueue) Close() {
	close(q.ch)
	q.signalFreed()
}

// Len returns the current packet count.
func (q *boundedQueue) Len() int { return len(q.ch) }

// Bytes returns the current summed raw datagram bytes.
func (q *boundedQueue) Bytes() int64 { return q.bytes.Load() }

// PeakBytes returns the high-water mark of held bytes across the run.
func (q *boundedQueue) PeakBytes() int64 { return q.peakBytes.Load() }
