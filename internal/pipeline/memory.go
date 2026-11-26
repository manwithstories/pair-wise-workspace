package pipeline

import (
	"context"
	"sync"

	"github.com/archive/tsrestore/internal/timeline"
)

// MaxInFlightBytes is the hard ceiling on queued plus in-flight buffered media.
const MaxInFlightBytes = 16 * 1024 * 1024 // 16 MiB

// memSemaphore is a byte-counted bounded buffer limiter built on a condition
// variable. Acquire blocks while the ceiling would be exceeded; that blocking
// is the backpressure signal, so a slow consumer stalls producers rather than
// causing silent drops. Cancellation wakes and unblocks every waiter.
type memSemaphore struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int64
	inUse    int64
	peak     int64
	closed   bool
	canceled bool
}

func newMemSemaphore(capacity int64) *memSemaphore {
	m := &memSemaphore{capacity: capacity}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Acquire reserves n bytes, honoring context cancellation. A reservation
// larger than the capacity is rejected as a fatal error rather than deadlocking.
func (m *memSemaphore) Acquire(ctx context.Context, n int) error {
	if int64(n) > m.capacity {
		return timeline.NewError(timeline.ClassFatal, "pipeline.memory",
			"single item exceeds in-flight capacity", nil)
	}
	// Wake waiters when the context is canceled.
	stop := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		m.canceled = true
		m.cond.Broadcast()
		m.mu.Unlock()
	})
	defer stop()

	m.mu.Lock()
	defer m.mu.Unlock()
	for m.inUse+int64(n) > m.capacity && !m.canceled {
		m.cond.Wait()
	}
	if m.canceled {
		return ctx.Err()
	}
	m.inUse += int64(n)
	if m.inUse > m.peak {
		m.peak = m.inUse
	}
	return nil
}

// Release returns n bytes to the pool and wakes one blocked producer.
func (m *memSemaphore) Release(n int) {
	m.mu.Lock()
	m.inUse -= int64(n)
	if m.inUse < 0 {
		m.inUse = 0
	}
	m.cond.Broadcast()
	m.mu.Unlock()
}

// PeakBytes reports the high-water mark of in-flight memory.
func (m *memSemaphore) PeakBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// InUse reports the currently reserved bytes.
func (m *memSemaphore) InUse() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inUse
}
