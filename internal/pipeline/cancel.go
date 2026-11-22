package pipeline

import (
	"context"
	"io"
	"sync"

	"medialine/internal/fault"
)

// CancelReason classifies why a run stopped early.
type CancelReason string

const (
	ReasonNone     CancelReason = ""
	ReasonSignal   CancelReason = "signal"
	ReasonDeadline CancelReason = "deadline"
	ReasonFault    CancelReason = "fault"
	ReasonClose    CancelReason = "close"
)

// Canceller fans one cancellation decision out to producers, the pool, the
// merger and writers, and records why it fired.
type Canceller struct {
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	mu     sync.Mutex
	reason CancelReason
}

// NewCanceller wraps a parent context.
func NewCanceller(parent context.Context) *Canceller {
	ctx, cancel := context.WithCancel(parent)
	return &Canceller{ctx: ctx, cancel: cancel}
}

// Context returns the cancellable context.
func (c *Canceller) Context() context.Context { return c.ctx }

// Cancel records the first reason and cancels.
func (c *Canceller) Cancel(reason CancelReason) {
	c.once.Do(func() {
		c.mu.Lock()
		c.reason = reason
		c.mu.Unlock()
		c.cancel()
	})
}

// CancelOnError cancels when err is non-nil and is not a recoverable fault.
func (c *Canceller) CancelOnError(err error) {
	if err == nil {
		return
	}
	if fault.IsRecoverable(err) {
		return
	}
	c.Cancel(ReasonFault)
}

// Canceled reports whether cancellation has fired.
func (c *Canceller) Canceled() bool { return c.ctx.Err() != nil }

// Reason returns the recorded reason, or ReasonNone when not cancelled.
func (c *Canceller) Reason() CancelReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// Err returns the cancellation fault, or nil.
func (c *Canceller) Err() error {
	if !c.Canceled() {
		return nil
	}
	return fault.New(fault.StageCancel, fault.KindCanceled, "run cancelled: %s", c.Reason())
}

// Guard wraps a blocking writer so cancellation unblocks it: callers that
// support deadlines can stop waiting on a stalled disk. The returned writer
// fails with a cancellation fault once the context is done, which lets the
// producer goroutine unwind and the file close.
type Guard struct {
	ctx context.Context
	w   io.WriteCloser
	// mu protects closed; Write and Close may run concurrently with cancel.
	mu     sync.Mutex
	closed bool
}

// NewGuard wraps w.
func NewGuard(ctx context.Context, w io.WriteCloser) *Guard {
	return &Guard{ctx: ctx, w: w}
}

// Write implements io.Writer with cancellation.
func (g *Guard) Write(p []byte) (int, error) {
	select {
	case <-g.ctx.Done():
		return 0, fault.From(g.ctx.Err(), fault.StageCancel, fault.KindCanceled)
	default:
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, fault.New(fault.StageWrite, fault.KindWrite, "writer already closed")
	}
	return g.w.Write(p)
}

// Close releases the underlying writer exactly once.
func (g *Guard) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	return g.w.Close()
}

// RecoverableOrCanceled reports whether err should stop a run: every error
// except an explicitly recoverable fragment fault does. Cancellation therefore
// stops the run, as do write and configuration faults.
func RecoverableOrCanceled(err error) bool {
	return err != nil && !fault.IsRecoverable(err)
}
