// Package stream provides a bounded pipeline: per-hop queues plus a
// global in-flight byte budget give backpressure against slow
// consumers, and cancellation aborts every stage with a recognizable
// status. Total in-flight memory never exceeds the configured budget
// (256 MiB in production use).
package stream

import (
	"context"
	"errors"
	"sync"
)

// ErrAborted marks a cancelled run; callers report it as an aborted
// status in their output.
var ErrAborted = errors.New("stream: pipeline aborted")

// Item is one unit of work; Size is its memory cost in bytes.
type Item struct {
	Window  int
	Size    int64
	Payload any
}

// Stage processes items from in and emits to out. A stage must select
// on ctx.Done() when receiving and sending, and return promptly on
// cancellation.
type Stage func(ctx context.Context, in <-chan Item, out chan<- Item) error

// Pipeline bounds queue depth and total in-flight bytes.
type Pipeline struct {
	Queue  int // per-hop buffer
	tokens chan struct{}
}

const tokenBytes = int64(1 << 20) // one token per MiB

// New creates a Pipeline with the given per-hop queue depth and total
// in-flight byte budget.
func New(queue int, maxBytes int64) *Pipeline {
	if queue < 0 {
		queue = 0
	}
	n := int(maxBytes / tokenBytes)
	if n < 1 {
		n = 1
	}
	return &Pipeline{Queue: queue, tokens: make(chan struct{}, n)}
}

func (p *Pipeline) cost(size int64) int {
	n := int((size + tokenBytes - 1) / tokenBytes)
	if n < 1 {
		n = 1
	}
	if n > cap(p.tokens) {
		n = cap(p.tokens)
	}
	return n
}

func (p *Pipeline) acquire(ctx context.Context, size int64) error {
	n := p.cost(size)
	for i := 0; i < n; i++ {
		select {
		case p.tokens <- struct{}{}:
		case <-ctx.Done():
			for j := 0; j < i; j++ {
				<-p.tokens
			}
			return ErrAborted
		}
	}
	return nil
}

func (p *Pipeline) release(size int64) {
	for i := 0; i < p.cost(size); i++ {
		<-p.tokens
	}
}

// Run wires src through the stages and drains the final output. A slow
// stage blocks producers once queues and the byte budget are full
// (backpressure). Cancellation aborts all stages and returns ErrAborted.
//
// A budget token is held exactly while an item waits in a managed hop
// buffer: each gate acquires on entry, each relay releases when the
// item leaves toward the next stage, and the final drain releases the
// last hop's tokens. Items in a stage's hands hold no budget; queued
// bytes never exceed the configured budget.
func (p *Pipeline) Run(ctx context.Context, src <-chan Item, stages ...Stage) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, len(stages))
	managed := make([]chan Item, 0, len(stages))
	var wg sync.WaitGroup
	in := src
	for i, st := range stages {
		raw := make(chan Item, p.Queue)   // stage output, unmanaged
		out := make(chan Item, p.Queue)   // managed: items here hold tokens
		rel := make(chan Item, p.Queue)   // stage input, tokens released
		managed = append(managed, out)
		idx, stage, stageIn := i, st, in
		wg.Add(3)
		go func() { // stage worker
			defer wg.Done()
			defer close(raw)
			if err := stage(ctx, stageIn, raw); err != nil {
				errs[idx] = err
			}
		}()
		go func() { // gate: acquire a token per item entering out
			defer wg.Done()
			defer close(out)
			aborted := false
			for it := range raw {
				if aborted {
					continue // drain so the stage can finish
				}
				if err := p.acquire(ctx, it.Size); err != nil {
					aborted = true
					continue
				}
				select {
				case out <- it:
				case <-ctx.Done():
					p.release(it.Size)
					aborted = true
				}
			}
		}()
		go func() { // relay: release the token as the item leaves out
			defer wg.Done()
			defer close(rel)
			for it := range out {
				p.release(it.Size)
				select {
				case rel <- it:
				case <-ctx.Done():
					return // leftovers in out are drained after wg.Wait
				}
			}
		}()
		in = rel
	}
	for it := range in { // final drain: items here hold no tokens
		_ = it
	}
	wg.Wait()
	// After abort, items stranded in managed buffers were never consumed
	// downstream; release their tokens. All channels are closed by now.
	for _, ch := range managed {
		for it := range ch {
			p.release(it.Size)
		}
	}
	if ctx.Err() != nil {
		return ErrAborted
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
