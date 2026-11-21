package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/rtp"

	"mediamerge/internal/fault"
)

// ConsumerOptions configure a sink that simulates a slow downstream writer.
type ConsumerOptions struct {
	// Delay is the simulated per-block write latency. It stands in for a
	// slow disk or downstream consumer without touching real devices.
	Delay time.Duration
	// FailAfter causes the sink to fail the Nth write (1-based). Zero
	// disables the failure.
	FailAfter int
}

// Sink is a consumer result.
type Sink struct {
	Blocks    int
	Bytes     int64
	Digests   [][32]byte
	FailWrite error
}

// Consume drains q until it is closed and empty. Every block is retained in
// the result's digest list, so a caller can prove that backpressure dropped
// nothing. Cancellation and write failures are returned with the segment
// number that failed.
func Consume(ctx context.Context, q *Queue, opts ConsumerOptions) (*Sink, error) {
	sink := &Sink{}
	for {
		b, err := q.Pop(ctx)
		if err != nil {
			return sink, err
		}
		if b == nil {
			return sink, nil
		}
		if opts.Delay > 0 {
			select {
			case <-time.After(opts.Delay):
			case <-ctx.Done():
				return sink, fault.Wrap(fault.StageStream, fault.CodeCanceled, ctx.Err(),
					fmt.Sprintf("consumer cancelled during segment %d", b.Segment))
			}
		}
		if opts.FailAfter > 0 && sink.Blocks+1 == opts.FailAfter {
			sink.FailWrite = fault.New(fault.StageStream, fault.CodeWriteFailed,
				fmt.Sprintf("simulated write failure at segment %d offset %d", b.Segment, b.Offset))
			return sink, sink.FailWrite
		}
		sink.Blocks++
		sink.Bytes += b.ByteSize()
		sink.Digests = append(sink.Digests, b.Digest)
	}
}

// Drain consumes q until it is closed and empty, invoking onBlock for every
// block. It mplements the same slow-consumer model as Consume and returns the
// number of blocks and bytes handled. onBlock returns an error to abort.
func Drain(ctx context.Context, q *Queue, opts ConsumerOptions, onBlock func(*Block) error) (int, int64, error) {
	var blocks int
	var bytes int64
	for {
		b, err := q.Pop(ctx)
		if err != nil {
			return blocks, bytes, err
		}
		if b == nil {
			return blocks, bytes, nil
		}
		if opts.Delay > 0 {
			select {
			case <-time.After(opts.Delay):
			case <-ctx.Done():
				return blocks, bytes, fault.Wrap(fault.StageStream, fault.CodeCanceled, ctx.Err(),
					fmt.Sprintf("consumer cancelled during segment %d", b.Segment))
			}
		}
		if opts.FailAfter > 0 && blocks+1 == opts.FailAfter {
			return blocks, bytes, fault.New(fault.StageStream, fault.CodeWriteFailed,
				fmt.Sprintf("simulated write failure at segment %d offset %d", b.Segment, b.Offset))
		}
		if err := onBlock(b); err != nil {
			return blocks, bytes, err
		}
		blocks++
		bytes += b.ByteSize()
	}
}

// BlockDigest is the digest identity used by tests: segment plus payload
// hash. Ordering is preserved by Consume.
type BlockDigest struct {
	Segment int
	Digest  [32]byte
}

// Digests returns the recorded block identities.
func (s *Sink) BlockDigests() []BlockDigest {
	out := make([]BlockDigest, 0, len(s.Digests))
	for i, d := range s.Digests {
		out = append(out, BlockDigest{Segment: i + 1, Digest: d})
	}
	return out
}

// RunPipe connects a producer to a queue and a consumer. It returns only when
// both goroutines have finished, so no goroutine is left blocked on a full or
// empty queue. The first error wins; the queue is always closed and both
// contexts are cancelled before returning.
//
// The producer is responsible for honouring ctx; the consumer is closed out
// by closing the queue after the producer returns.
func RunPipe(ctx context.Context, q *Queue, produce func(context.Context) error, opts ConsumerOptions) (*Sink, error) {
	return RunPipeWith(ctx, q, produce, func(cctx context.Context) (*Sink, error) {
		return Consume(cctx, q, opts)
	}, opts)
}

// RunPipeWith is RunPipe with a caller-supplied consumer, so a pipeline can
// write blocks to their final location as they arrive instead of buffering
// them all. The consumer must stop when the context is cancelled.
func RunPipeWith(ctx context.Context, q *Queue, produce func(context.Context) error,
	consume func(context.Context) (*Sink, error), opts ConsumerOptions) (*Sink, error) {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var prodErr, consErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		prodErr = produce(ctx)
		q.Close()
	}()
	sink := &Sink{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sink, consErr = consume(ctx)
	}()
	wg.Wait()
	cancel()

	if prodErr != nil && !errors.Is(prodErr, context.Canceled) {
		return sink, prodErr
	}
	if consErr != nil {
		return sink, consErr
	}
	return sink, prodErr
}

// NewPacket builds the RTP packet for a block. Timestamp is in the track's
// output sample rate so a downstream muxer can re-stamp losslessly.
func NewPacket(seq uint16, timestamp uint32, ssrc uint32, payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      timestamp,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
}
