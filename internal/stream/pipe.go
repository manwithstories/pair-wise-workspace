// Package stream provides a bounded dispatch queue with backpressure and a
// memory budget so the pipeline never buffers the whole programme.
package stream

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed is returned when sending to a closed pipe.
var ErrClosed = errors.New("stream: pipe closed")

// Pipe is a bounded queue. When it is full, Send blocks, applying
// backpressure to the producer. Cancelling the context or closing the pipe
// stops dispatch; items already delivered remain completed.
type Pipe[T any] struct {
	ch     chan T
	done   chan struct{}
	mu     sync.RWMutex
	closed bool
}

// NewPipe creates a pipe holding at most capacity items.
func NewPipe[T any](capacity int) *Pipe[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &Pipe[T]{ch: make(chan T, capacity), done: make(chan struct{})}
}

// Send enqueues t, blocking while the queue is full. It returns an error if
// the context is cancelled or the pipe is closed first.
func (p *Pipe[T]) Send(ctx context.Context, t T) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	select {
	case p.ch <- t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrClosed
	}
}

// Recv dequeues the next item. ok is false once the pipe is closed and
// drained, or the context is cancelled.
func (p *Pipe[T]) Recv(ctx context.Context) (t T, ok bool) {
	select {
	case t, ok = <-p.ch:
		return t, ok
	case <-ctx.Done():
		return t, false
	}
}

// Close stops dispatch. Items already queued stay available to Recv.
func (p *Pipe[T]) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.done)
		close(p.ch)
	}
}
