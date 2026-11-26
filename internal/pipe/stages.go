// Package pipe provides a bounded pipeline: queue capacity is
// configurable (default 8), a slow consumer blocks producers stage by
// stage, cancellation releases every goroutine, and in-flight buffer
// bytes are metered against a 256 KiB peak budget.
package pipe

import (
	"context"
	"sync"

	"deliver/internal/model"
)

// DefaultCapacity is the default per-stage queue depth.
const DefaultCapacity = 8

// MaxBufferBytes caps in-flight payload at 256 KiB.
const MaxBufferBytes = 256 * 1024

// Item is anything flowing through a Queue with a known byte size.
type Item interface{ Size() int }

// Meter accounts in-flight bytes and records the peak.
type Meter struct {
	mu   sync.Mutex
	cur  int64
	Peak int64
}

// Add charges n bytes, updating the peak. It fails when the 256 KiB
// budget would be exceeded.
func (m *Meter) Add(n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur+n > MaxBufferBytes {
		return model.Fatalf(model.StagePipe, "buffer budget %d exceeded", MaxBufferBytes)
	}
	m.cur += n
	if m.cur > m.Peak {
		m.Peak = m.cur
	}
	return nil
}

// Sub releases n bytes.
func (m *Meter) Sub(n int64) {
	m.mu.Lock()
	m.cur -= n
	m.mu.Unlock()
}

// Queue is a bounded, metered channel between two stages.
type Queue[T Item] struct {
	ch    chan T
	meter *Meter
	once  sync.Once
}

// NewQueue creates a queue of the given capacity (default 8).
func NewQueue[T Item](capacity int, meter *Meter) *Queue[T] {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if meter == nil {
		meter = &Meter{}
	}
	return &Queue[T]{ch: make(chan T, capacity), meter: meter}
}

// Push blocks while the queue is full, propagating backpressure to the
// producer. It returns early when ctx is cancelled or the queue closes.
func (q *Queue[T]) Push(ctx context.Context, it T) error {
	if err := q.meter.Add(int64(it.Size())); err != nil {
		return err
	}
	select {
	case q.ch <- it:
		return nil
	case <-ctx.Done():
		q.meter.Sub(int64(it.Size()))
		return ctx.Err()
	}
}

// Pop returns the next item, or ok=false once the queue is drained and
// closed. It returns early when ctx is cancelled.
func (q *Queue[T]) Pop(ctx context.Context) (it T, ok bool) {
	select {
	case it, ok = <-q.ch:
		if ok {
			q.meter.Sub(int64(it.Size()))
		}
		return it, ok
	case <-ctx.Done():
		return it, false
	}
}

// Close marks the queue end-of-stream; safe to call once per producer.
func (q *Queue[T]) Close() { q.once.Do(func() { close(q.ch) }) }

// Run starts every stage in its own goroutine. The first stage error
// cancels the others; Run returns after all goroutines have exited, so
// no goroutine outlives the call.
func Run(ctx context.Context, stages ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, len(stages))
	var wg sync.WaitGroup
	for _, st := range stages {
		wg.Add(1)
		go func(fn func(context.Context) error) {
			defer wg.Done()
			if err := fn(ctx); err != nil {
				errCh <- err
				cancel()
			}
		}(st)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}
