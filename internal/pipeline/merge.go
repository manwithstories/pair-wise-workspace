package pipeline

import (
	"context"
)

// Merger re-orders worker results strictly by input sequence number:
// out-of-order completion never changes the emitted order, so reports,
// segment boundaries and hashes are deterministic. A failed job is
// emitted in its place (with Err set and no events); the remaining
// results still merge in order and the first error is recorded.
type Merger struct {
	next     uint64
	pending  map[uint64]Result
	firstErr error
}

func NewMerger() *Merger {
	return &Merger{pending: make(map[uint64]Result)}
}

// Add stores r and returns every result now ready in sequence order.
func (m *Merger) Add(r Result) []Result {
	if r.Err != nil && m.firstErr == nil {
		m.firstErr = r.Err
	}
	m.pending[r.Seq] = r
	var out []Result
	for {
		r, ok := m.pending[m.next]
		if !ok {
			return out
		}
		delete(m.pending, m.next)
		m.next++
		out = append(out, r)
	}
}

// FirstErr returns the first job error observed, if any.
func (m *Merger) FirstErr() error { return m.firstErr }

// Ordered runs a merger goroutine: it consumes in (until closed or ctx
// cancelled) and emits results in input order on the returned channel.
func Ordered(ctx context.Context, in <-chan Result) <-chan Result {
	out := make(chan Result)
	go func() {
		defer close(out)
		m := NewMerger()
		for {
			select {
			case <-ctx.Done():
				return
			case r, ok := <-in:
				if !ok {
					return
				}
				for _, ready := range m.Add(r) {
					select {
					case out <- ready:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out
}
