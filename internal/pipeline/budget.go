package pipeline

import (
	"sync"

	"mediad/internal/codec"
	"mediad/internal/timeline"
)

// MemoryLimitBytes is the process pipeline's edge-node memory budget.
const MemoryLimitBytes int64 = 64 << 20

// byteBudget is a weighted bounded-memory gate. Producers pause when admitted
// bytes would exceed the edge-node limit.
type byteBudget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	max   int64
	used  int64
	waits int64
}

func newByteBudget(max int64) *byteBudget {
	b := &byteBudget{max: max}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *byteBudget) acquire(n int64) error {
	if n < 0 {
		return timeline.New(timeline.KindInternal, "pipeline.memory", -1, -1, "negative byte budget request")
	}
	if n > codec.MaxAUBytes {
		return timeline.New(timeline.KindArgument, "pipeline.memory", -1, -1,
			"access unit %d exceeds single-AU limit %d", n, codec.MaxAUBytes)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used+n > b.max {
		b.waits++
		b.cond.Wait()
	}
	b.used += n
	return nil
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *byteBudget) stats() (used, waits int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.waits
}
