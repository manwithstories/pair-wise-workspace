// Package stream provides a bounded sample pipe with explicit backpressure.
// Memory residency never exceeds the configured queue capacity plus the
// samples currently being handed between producer and consumer.
package stream

import (
	"context"
	"errors"
	"sync"

	"mediamerge/internal/model"
)

// Statistics reports queue occupancy and progress.
type Statistics struct {
	Capacity int
	Queued   int
	Sent     int
	Received int
	Resident int
}

// Pipe is a bounded channel of samples. Sends block once the queue is full,
// which applies backpressure to the demux/read stage instead of dropping
// samples. A single producer and a single consumer are expected.
type Pipe struct {
	ch   chan model.Sample
	ctx  context.Context
	stop context.CancelFunc

	mu           sync.Mutex
	err          error
	consumerDone bool
	producerDone bool
	sent         int
	received     int
	boundary     model.Boundary
}

// New creates a pipe with the given queue capacity. Capacity is clamped to at
// least one so a slow consumer can never cause unbounded buffering.
func New(ctx context.Context, capacity int) *Pipe {
	if capacity < 1 {
		capacity = 1
	}
	pctx, stop := context.WithCancel(ctx)
	return &Pipe{ch: make(chan model.Sample, capacity), ctx: pctx, stop: stop}
}

// Capacity returns the configured queue depth.
func (p *Pipe) Capacity() int { return cap(p.ch) }

// Send enqueues a sample, blocking while the queue is full. It fails when the
// context is canceled, when the consumer has signaled an error, or after the
// consumer has stopped receiving.
func (p *Pipe) Send(s model.Sample) error {
	p.mu.Lock()
	if err := p.err; err != nil {
		p.mu.Unlock()
		return err
	}
	if p.consumerDone {
		p.mu.Unlock()
		return model.NewError(model.StageStream, model.CodeConsumerGone, s.Offset, s.TrackID, true,
			"consumer exited before sample %d", s.Index)
	}
	if p.producerDone {
		p.mu.Unlock()
		return model.NewError(model.StageStream, model.CodeStream, s.Offset, s.TrackID, true,
			"send after CloseSend for sample %d", s.Index)
	}
	p.mu.Unlock()

	select {
	case <-p.ctx.Done():
		return p.wrapCancel(s, p.ctx.Err())
	case p.ch <- s:
		p.mu.Lock()
		p.sent++
		p.mu.Unlock()
		return nil
	}
}

// Recv dequeues the next sample. ok is false after the producer closes the
// pipe.
func (p *Pipe) Recv() (model.Sample, bool, error) {
	p.mu.Lock()
	if err := p.err; err != nil && (p.consumerDone || p.producerDone) {
		p.mu.Unlock()
		return model.Sample{}, false, err
	}
	p.mu.Unlock()
	// Prefer draining buffered samples over observing a cancellation that may
	// have been triggered by the producer finishing its side.
	select {
	case s, ok := <-p.ch:
		return p.deliver(s, ok)
	default:
	}
	select {
	case <-p.ctx.Done():
		select {
		case s, ok := <-p.ch:
			return p.deliver(s, ok)
		default:
		}
		b := p.Boundary()
		return model.Sample{}, false, &model.Error{
			Stage: model.StageStream, Code: model.CodeCanceled, Offset: b.OutputBytes,
			TrackID: b.TrackID, Recoverable: true, Boundary: &b,
			Message: "stream canceled", Cause: p.ctx.Err(),
		}
	case s, ok := <-p.ch:
		return p.deliver(s, ok)
	}
}

func (p *Pipe) deliver(s model.Sample, ok bool) (model.Sample, bool, error) {
	if !ok {
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		return model.Sample{}, false, err
	}
	// The boundary is owned by the consumer: it must only advance after the
	// sample has been fully written. Recording it here would report a sample
	// as complete before the write that could still fail.
	p.mu.Lock()
	p.received++
	p.mu.Unlock()
	return s, true, nil
}

// CloseSend closes the queue. The producer must not send afterwards. Buffered
// samples remain readable, so a finished producer never truncates the tail.
func (p *Pipe) CloseSend() {
	p.mu.Lock()
	if p.producerDone {
		p.mu.Unlock()
		return
	}
	p.producerDone = true
	p.mu.Unlock()
	close(p.ch)
}

// Fail records a terminal error and unblocks both sides. It is idempotent.
func (p *Pipe) Fail(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.err == nil {
		if pe, ok := err.(*model.Error); ok && pe.Boundary == nil {
			b := p.boundary
			pe.Boundary = &b
		}
		p.err = err
	}
	p.consumerDone = true
	p.mu.Unlock()
	p.stop()
}

// Close releases the pipe context. Call after both sides are done.
func (p *Pipe) Close() { p.stop() }

// Err returns the terminal error, if any.
func (p *Pipe) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// SetBoundary records the last fully written sample and output byte offset.
func (p *Pipe) SetBoundary(b model.Boundary) {
	p.mu.Lock()
	p.boundary = b
	p.mu.Unlock()
}

// Boundary returns the last fully processed sample boundary.
func (p *Pipe) Boundary() model.Boundary {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.boundary
}

// Stats snapshots queue state. Resident counts queued samples plus at most one
// sample in flight in each direction.
func (p *Pipe) Stats() Statistics {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := len(p.ch)
	inflight := 0
	if q > 0 {
		inflight = 2
	} else if p.sent > p.received {
		inflight = 1
	}
	return Statistics{
		Capacity: cap(p.ch), Queued: q, Sent: p.sent, Received: p.received,
		Resident: q + inflight,
	}
}

func (p *Pipe) wrapCancel(s model.Sample, cause error) error {
	b := p.Boundary()
	return &model.Error{
		Stage: model.StageStream, Code: model.CodeCanceled, Offset: s.Offset, TrackID: s.TrackID,
		Recoverable: true, Boundary: &b, Message: "send canceled", Cause: cause,
	}
}

// ErrConsumerGone is returned by producers that detect an early consumer exit.
var ErrConsumerGone = errors.New("stream: consumer gone")
