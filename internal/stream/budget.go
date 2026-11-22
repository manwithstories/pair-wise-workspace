package stream

import (
	"errors"
	"fmt"
	"sync"
)

// ErrBudgetExceeded is returned when a reservation would exceed the budget.
var ErrBudgetExceeded = errors.New("stream: memory budget exceeded")

// Budget is a concurrency-safe accounting of reserved bytes with a hard cap.
type Budget struct {
	mu    sync.Mutex
	limit int64
	used  int64
	peak  int64
}

// NewBudget creates a budget of limit bytes.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// Reserve claims n bytes. It fails without side effects when the
// reservation would exceed the limit.
func (b *Budget) Reserve(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return fmt.Errorf("%w: need %d, have %d of %d", ErrBudgetExceeded, n, b.limit-b.used, b.limit)
	}
	b.used += n
	if b.used > b.peak {
		b.peak = b.used
	}
	return nil
}

// Release returns n bytes previously reserved.
func (b *Budget) Release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
}

// Peak returns the high-water mark of reserved bytes.
func (b *Budget) Peak() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// Limit returns the configured cap.
func (b *Budget) Limit() int64 { return b.limit }
