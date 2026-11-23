// Package stream provides the bounded in-memory pipeline that connects
// producers and consumers with backpressure. Memory is capped by a byte
// limit: a slow consumer blocks the producer instead of letting buffers
// grow. Cancellation, write failure and downstream early close all release
// the producer with a locatable error.
package stream

import (
	"context"
	"io"
	"sync"

	"archive/internal/media"
)

// Pipe is a bounded byte-chunk queue with backpressure. State changes are
// broadcast by closing notify and replacing it, so waiters can also select
// on context cancellation.
type Pipe struct {
	mu        sync.Mutex
	notify    chan struct{}
	queue     [][]byte
	inFlight  int64
	byteLimit int64
	maxChunks int
	closed    bool
	closeErr  error // terminal error from the producer (nil = clean EOF)
	abortErr  error // downstream went away
	peak      int64
}

// NewPipe creates a pipe bounded by byteLimit bytes and maxChunks chunks.
func NewPipe(byteLimit int64, maxChunks int) *Pipe {
	return &Pipe{notify: make(chan struct{}), byteLimit: byteLimit, maxChunks: maxChunks}
}

// signal wakes all waiters. Caller must hold p.mu.
func (p *Pipe) signal() {
	close(p.notify)
	p.notify = make(chan struct{})
}

// Send blocks until the chunk fits (backpressure) and returns a locatable
// error on cancellation, abort or producer-side close.
func (p *Pipe) Send(ctx context.Context, b []byte) error {
	for {
		p.mu.Lock()
		if p.abortErr != nil {
			p.mu.Unlock()
			return p.abortErr
		}
		if p.closed {
			p.mu.Unlock()
			return media.New(media.KindCanceled, "stream.pipe", "", -1,
				"send on closed pipe", nil)
		}
		if len(p.queue) < p.maxChunks && p.inFlight+int64(len(b)) <= p.byteLimit {
			p.queue = append(p.queue, b)
			p.inFlight += int64(len(b))
			if p.inFlight > p.peak {
				p.peak = p.inFlight
			}
			p.signal()
			p.mu.Unlock()
			return nil
		}
		ch := p.notify
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return media.New(media.KindCanceled, "stream.pipe", "", -1,
				"send canceled", ctx.Err())
		}
	}
}

// Recv returns the next chunk, io.EOF on clean close, or the terminal
// error. A canceled context yields a locatable canceled error.
func (p *Pipe) Recv(ctx context.Context) ([]byte, error) {
	for {
		p.mu.Lock()
		if p.abortErr != nil {
			p.mu.Unlock()
			return nil, p.abortErr
		}
		if len(p.queue) > 0 {
			b := p.queue[0]
			p.queue = p.queue[1:]
			p.inFlight -= int64(len(b))
			p.signal()
			p.mu.Unlock()
			return b, nil
		}
		if p.abortErr != nil {
			p.mu.Unlock()
			return nil, p.abortErr
		}
		if p.closed {
			err := p.closeErr
			p.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		ch := p.notify
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, media.New(media.KindCanceled, "stream.pipe", "", -1,
				"recv canceled", ctx.Err())
		}
	}
}

// Close marks the producer done; err (if any) becomes the terminal error
// delivered to consumers after the queue drains.
func (p *Pipe) Close(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		p.closeErr = err
		p.signal()
	}
}

// Abort releases everyone because the downstream closed early. Producers
// blocked in Send wake up with a locatable error.
func (p *Pipe) Abort(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.abortErr == nil {
		p.abortErr = media.New(media.KindCanceled, "stream.pipe", "", -1,
			"downstream closed early", err)
		p.signal()
	}
}

// Peak returns the peak in-flight bytes (memory watermark).
func (p *Pipe) Peak() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}
