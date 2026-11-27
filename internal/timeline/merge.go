// Package timeline merges per-track event streams into one stable,
// deduplicated, watermark-guarded timeline.
package timeline

import (
	"container/heap"
	"context"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

// RecvFunc pulls the next event from a source; ok=false means drained.
type RecvFunc func(ctx context.Context) (ev model.Event, ok bool, err error)

type item struct {
	ev  model.Event
	src int
}

// minHeap orders by (start time, track, source offset) so that
// simultaneous events have a deterministic, stable order.
type minHeap []item

func (h minHeap) Len() int { return len(h) }

func (h minHeap) Less(i, j int) bool {
	a, b := h[i].ev, h[j].ev
	if c := a.Start.Compare(b.Start); c != 0 {
		return c < 0
	}
	if a.Track != b.Track {
		return a.Track < b.Track
	}
	return a.Offset < b.Offset
}

func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *minHeap) Pop() (v any)      { old := *h; v = old[len(old)-1]; *h = old[:len(old)-1]; return }

func sameEvent(a, b model.Event) bool {
	return a.Track == b.Track && a.Kind == b.Kind &&
		a.Start.Compare(b.Start) == 0 && a.Payload == b.Payload
}

// Merge k-way merges sorted sources into a single ordered stream.
// Duplicate events are dropped; events arriving behind the watermark
// are reported through late (when non-nil) and excluded.
func Merge(ctx context.Context, recvs []RecvFunc, send func(context.Context, model.Event) error, late func(model.TrackError)) error {
	h := &minHeap{}
	for i, recv := range recvs {
		ev, ok, err := recv(ctx)
		if err != nil {
			return err
		}
		if ok {
			heap.Push(h, item{ev: ev, src: i})
		}
	}
	var watermark clock.Time
	hasWM := false
	var last model.Event
	hasLast := false
	for h.Len() > 0 {
		it := heap.Pop(h).(item)
		if nxt, ok, err := recvs[it.src](ctx); err != nil {
			return err
		} else if ok {
			heap.Push(h, item{ev: nxt, src: it.src})
		}
		ev := it.ev
		if hasWM && ev.Start.Compare(watermark) < 0 {
			if late != nil {
				late(model.TrackError{Track: ev.Track, Stage: model.StageMerge,
					Offset: ev.Offset, Time: ev.Start,
					Msg: "late event behind watermark " + watermark.String()})
			}
			continue
		}
		if hasLast && sameEvent(last, ev) {
			continue
		}
		if err := send(ctx, ev); err != nil {
			return err
		}
		watermark, hasWM = ev.Start, true
		last, hasLast = ev, true
	}
	return nil
}
