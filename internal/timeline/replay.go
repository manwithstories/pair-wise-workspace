package timeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"mediqc/internal/media"
)

// Event is one timestamped media sample or timeline control observation.
type Event struct {
	ID         string          `json:"id"`
	TraceID    string          `json:"trace_id,omitempty"`
	TrackID    int             `json:"track_id"`
	Sequence   int             `json:"sequence"`
	EventMS    int64           `json:"event_ms"`
	Arrival    int64           `json:"arrival"`
	ChunkIndex int             `json:"chunk_index"`
	Ref        media.SampleRef `json:"ref"`
}

// ScheduledEvent preserves the original event time and variable interval.
type ScheduledEvent struct {
	Event
	IntervalMS int64 `json:"interval_ms"`
}

// Outcome records deterministic late-event handling.
type Outcome struct {
	Late           bool   `json:"late"`
	Replayed       bool   `json:"replayed"`
	Unfixable      bool   `json:"unfixable"`
	CommittedChunk bool   `json:"committed_chunk"`
	ReplayCount    int    `json:"replay_count"`
	HashBefore     string `json:"hash_before,omitempty"`
	HashAfter      string `json:"hash_after,omitempty"`
	HashStable     bool   `json:"hash_stable"`
}

// Replayer owns the watermark and bounded retrospective event window.
type Replayer struct {
	WM              *Watermark
	RetentionMS     int64
	MaxEvents       int
	ChunkDurationMS int64
	events          []Event
	committed       map[int]string
	ReplayCount     int `json:"late_replays"`
	UnfixableGaps   int `json:"unfixable_gaps"`
	Evicted         int `json:"evicted"`
}

// NewReplayer constructs a bounded replayer.
func NewReplayer(retentionMS int64, maxEvents int) *Replayer {
	if retentionMS <= 0 {
		retentionMS = 5000
	}
	if maxEvents <= 0 {
		maxEvents = 256
	}
	return &Replayer{
		WM:              NewWatermark(0),
		RetentionMS:     retentionMS,
		MaxEvents:       maxEvents,
		ChunkDurationMS: 1000,
		committed:       make(map[int]string),
	}
}

// ChunkFor returns the deterministic chunk index for an event time.
func (r *Replayer) ChunkFor(eventMS int64) int {
	if r.ChunkDurationMS <= 0 {
		return 0
	}
	return int(eventMS / r.ChunkDurationMS)
}

// CommitChunk records an immutable output chunk hash.
func (r *Replayer) CommitChunk(index int, hash string) error {
	if old, ok := r.committed[index]; ok && old != hash {
		return fmt.Errorf("timeline: committed chunk %d hash changed", index)
	}
	r.committed[index] = hash
	return nil
}

// IsChunkCommitted reports whether a chunk is already immutable.
func (r *Replayer) IsChunkCommitted(index int) bool {
	_, ok := r.committed[index]
	return ok
}

// Add accepts an arrival-order event and replays when it is safely retained.
func (r *Replayer) Add(ev Event) Outcome {
	ev.ChunkIndex = r.ChunkFor(ev.EventMS)
	late := r.WM.IsLate(ev.EventMS)
	out := Outcome{Late: late}
	before := r.Hash()

	if late {
		lag := r.WM.Current() - ev.EventMS
		if lag > r.RetentionMS {
			r.UnfixableGaps++
			out.Unfixable = true
			return out
		}
		if len(r.events) >= r.MaxEvents {
			r.prune()
		}
		if len(r.events) >= r.MaxEvents {
			r.UnfixableGaps++
			out.Unfixable = true
			return out
		}
		r.events = append(r.events, ev)
		r.ReplayCount++
		out.Replayed = true
		out.ReplayCount = r.ReplayCount
		out.HashBefore = before
		out.HashAfter = r.Hash()
		out.HashStable = out.HashAfter == r.Hash()
		out.CommittedChunk = r.IsChunkCommitted(ev.ChunkIndex)
		r.prune()
		_, _ = r.WM.Observe(max64(r.WM.Current(), ev.EventMS))
		return out
	}

	r.events = append(r.events, ev)
	if len(r.events) > r.MaxEvents {
		r.prune()
	}
	_, _ = r.WM.Observe(ev.EventMS)
	out.HashAfter = r.Hash()
	out.HashStable = out.HashAfter == r.Hash()
	return out
}

// Schedule returns events sorted by media time and preserves VFR intervals.
func (r *Replayer) Schedule() []ScheduledEvent {
	events := append([]Event(nil), r.events...)
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].EventMS != events[j].EventMS {
			return events[i].EventMS < events[j].EventMS
		}
		if events[i].TrackID != events[j].TrackID {
			return events[i].TrackID < events[j].TrackID
		}
		return events[i].Sequence < events[j].Sequence
	})
	last := map[int]int64{}
	out := make([]ScheduledEvent, 0, len(events))
	for _, ev := range events {
		se := ScheduledEvent{Event: ev}
		if prev, ok := last[ev.TrackID]; ok && ev.EventMS >= prev {
			se.IntervalMS = ev.EventMS - prev
		}
		last[ev.TrackID] = ev.EventMS
		out = append(out, se)
	}
	return out
}

// Hash computes a deterministic digest of the retained schedule.
func (r *Replayer) Hash() string {
	h := sha256.New()
	for _, ev := range r.Schedule() {
		fmt.Fprintf(h, "%s|%d|%d|%d|%d|%s|%d\n",
			ev.ID, ev.TrackID, ev.Sequence, ev.EventMS, ev.IntervalMS,
			ev.Ref.SHA256, ev.Ref.Duration)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Replayer) prune() {
	if len(r.events) == 0 {
		return
	}
	floor := r.WM.Current() - r.RetentionMS
	kept := r.events[:0]
	for _, ev := range r.events {
		if ev.EventMS >= floor {
			kept = append(kept, ev)
		} else {
			r.Evicted++
		}
	}
	r.events = kept
	for len(r.events) > r.MaxEvents {
		r.events = append(r.events[:0], r.events[1:]...)
		r.Evicted++
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
