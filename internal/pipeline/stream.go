// Package pipeline wires stages together with bounded queues,
// backpressure, cancellation and a global buffered-byte budget.
package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"cueforge/internal/model"
)

// ErrBudgetExceeded reports that buffered bytes exceeded the budget.
var ErrBudgetExceeded = errors.New("pipeline: buffered byte budget exceeded")

// Budget tracks total bytes held in queues and the observed peak.
type Budget struct {
	limit int64
	cur   atomic.Int64
	peak  atomic.Int64
}

func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// Add reserves n bytes, refusing beyond the limit.
func (b *Budget) Add(n int64) error {
	cur := b.cur.Add(n)
	for {
		p := b.peak.Load()
		if cur <= p || b.peak.CompareAndSwap(p, cur) {
			break
		}
	}
	if cur > b.limit {
		b.cur.Add(-n)
		return ErrBudgetExceeded
	}
	return nil
}

// Sub releases n bytes.
func (b *Budget) Sub(n int64) { b.cur.Add(-n) }

// Peak returns the maximum simultaneously buffered byte count.
func (b *Budget) Peak() int64 { return b.peak.Load() }

// Queue is a bounded event queue with budget accounting.
type Queue struct {
	ch   chan model.Event
	b    *Budget
	once sync.Once
}

func NewQueue(capacity int, b *Budget) *Queue {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue{ch: make(chan model.Event, capacity), b: b}
}

// Send blocks until the event is queued or ctx is cancelled; the
// blocked send is what applies backpressure to the producer.
func (q *Queue) Send(ctx context.Context, ev model.Event) error {
	if err := q.b.Add(ev.Size()); err != nil {
		return err
	}
	select {
	case q.ch <- ev:
		return nil
	case <-ctx.Done():
		q.b.Sub(ev.Size())
		return ctx.Err()
	}
}

// Recv returns the next event; ok=false means the queue is closed
// and drained.
func (q *Queue) Recv(ctx context.Context) (model.Event, bool, error) {
	select {
	case ev, ok := <-q.ch:
		if !ok {
			return model.Event{}, false, nil
		}
		q.b.Sub(ev.Size())
		return ev, true, nil
	case <-ctx.Done():
		return model.Event{}, false, ctx.Err()
	}
}

// Close closes the queue; safe to call more than once.
func (q *Queue) Close() { q.once.Do(func() { close(q.ch) }) }

// Run starts each stage in its own goroutine and waits for all.
// Cancellation of a sibling stage is a consequence, not a cause, so
// a non-cancellation error is preferred when several stages fail.
func Run(stages ...func() error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(stages))
	for i, st := range stages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = st()
		}()
	}
	wg.Wait()
	var canceled error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			if canceled == nil {
				canceled = err
			}
			continue
		}
		return err
	}
	return canceled
}
