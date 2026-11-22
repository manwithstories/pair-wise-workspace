package pipeline

import (
	"context"
	"sort"

	"medialine/internal/fault"
)

// MergeKey orders merged results on the unified timeline. Track breaks ties so
// two representations that share a boundary still have a defined order.
type MergeKey struct {
	StartUS int64
	TrackID int
	RepID   string
	Seq     int64
}

// Less compares two keys deterministically.
func (k MergeKey) Less(o MergeKey) bool {
	if k.StartUS != o.StartUS {
		return k.StartUS < o.StartUS
	}
	if k.TrackID != o.TrackID {
		return k.TrackID < o.TrackID
	}
	if k.RepID != o.RepID {
		return k.RepID < o.RepID
	}
	return k.Seq < o.Seq
}

// Result pairs a merged value with its ordering key and any fault. Faults do
// not remove a result: the caller decides whether the value is usable.
type Result[T any] struct {
	Key   MergeKey
	Value T
	Err   error
}

// Merger collects results from concurrent workers and emits them in a stable
// order. Results may arrive in any order; the emitted order depends only on
// the keys, so repeated runs over the same input produce byte-identical
// manifests.
type Merger[T any] struct {
	results []Result[T]
	ctx     context.Context
}

// NewMerger binds a merger to a cancellation context.
func NewMerger[T any](ctx context.Context) *Merger[T] {
	return &Merger[T]{ctx: ctx}
}

// Add records one result. It is safe to call from a worker only through the
// caller's own synchronisation; Merger itself is not a concurrent container.
func (m *Merger[T]) Add(r Result[T]) {
	m.results = append(m.results, r)
}

// Len reports how many results have been added.
func (m *Merger[T]) Len() int { return len(m.results) }

// Ordered returns results sorted by key. The input slice is not modified.
func (m *Merger[T]) Ordered() []Result[T] {
	out := append([]Result[T](nil), m.results...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key.Less(out[j].Key) })
	return out
}

// Emit streams ordered results on ch, stopping when the context is cancelled.
// It never closes ch: the producer owns that. A cancelled context causes an
// immediate return so the emitting goroutine cannot leak.
func (m *Merger[T]) Emit(ctx context.Context, ch chan<- Result[T]) error {
	for _, r := range m.Ordered() {
		if err := ctx.Err(); err != nil {
			return fault.From(err, fault.StageCancel, fault.KindCanceled)
		}
		select {
		case ch <- r:
		case <-ctx.Done():
			return fault.From(ctx.Err(), fault.StageCancel, fault.KindCanceled)
		}
	}
	return nil
}

// Dedup removes results whose key collides with an earlier one. With stable
// ordering, the first occurrence is retained; this makes "resume after
// cancellation" idempotent so completed fragments are not counted twice.
func (m *Merger[T]) Dedup() int {
	ordered := m.Ordered()
	seen := make(map[string]bool, len(ordered))
	out := make([]Result[T], 0, len(ordered))
	removed := 0
	for _, r := range ordered {
		k := keyString(r.Key)
		if seen[k] {
			removed++
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	m.results = out
	return removed
}

func keyString(k MergeKey) string {
	return k.RepID + "|" + itoa(k.TrackID) + "|" + itoa64(k.StartUS)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
