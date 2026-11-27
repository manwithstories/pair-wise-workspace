// Package timeline maintains the global watermark across tracks and
// replays deterministically from checkpoints when late packets arrive
// inside the rollback horizon.
package timeline

import "archiver/internal/media"

// Event is one media event on the shared timeline.
type Event struct {
	Track  media.TrackID
	TimeUs media.TimeUs
	ExtSeq uint64 // extended RTP sequence number for stable ordering
	Data   []byte
}

// Watermark tracks the maximum observed event time per track and
// advances the global watermark to min(track maxima) - lateness. The
// watermark never moves backwards.
type Watermark struct {
	lateness media.TimeUs
	maxEvent map[media.TrackID]media.TimeUs
	cur      media.TimeUs
	started  bool
}

func NewWatermark(lateness media.TimeUs) *Watermark {
	return &Watermark{lateness: lateness, maxEvent: map[media.TrackID]media.TimeUs{}}
}

// Observe records an event time for a track and returns the current
// watermark.
func (w *Watermark) Observe(t media.TrackID, ts media.TimeUs) media.TimeUs {
	if ts > w.maxEvent[t] {
		w.maxEvent[t] = ts
	}
	var min media.TimeUs
	first := true
	for _, v := range w.maxEvent {
		if first || v < min {
			min, first = v, false
		}
	}
	if !first {
		if cand := min - w.lateness; !w.started || cand > w.cur {
			w.cur, w.started = cand, true
		}
	}
	return w.cur
}

// Current returns the current watermark and whether any event was seen.
func (w *Watermark) Current() (media.TimeUs, bool) { return w.cur, w.started }
