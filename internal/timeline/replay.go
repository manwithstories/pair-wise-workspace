package timeline

import (
	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// Replayer accepts events that arrive after the watermark sealed their slot.
// It keeps a fixed-width history of sealed output and, when a late event falls
// inside the replay window, recomputes the affected interval deterministically
// so repeated runs yield identical frame hashes and metrics.
type Replayer struct {
	window   clock.Ticks
	history  []SealedItem
	log      *model.Log
	replays  int
	rejected int
	maxHist  int
}

// SealedItem is one stable output record retained for replay.
type SealedItem struct {
	At     clock.Ticks
	Stream uint32
	Seq    uint16
	Hash   uint64
	Order  int
}

// NewReplayer creates a replayer. replayWindow defines how far behind the
// watermark a late event may still trigger recomputation; historyCap bounds
// retained memory regardless of stream length.
func NewReplayer(replayWindow clock.Ticks, historyCap int, log *model.Log) *Replayer {
	if historyCap <= 0 {
		historyCap = 4096
	}
	return &Replayer{window: replayWindow, log: log, maxHist: historyCap}
}

// Decision is the outcome of offering one event to the replayer.
type Decision struct {
	// Replay is true when the event falls inside the replay window and the
	// sealed interval must be recomputed.
	Replay bool
	// Rejected is true when the event is older than the replay window and is
	// permanently discarded.
	Rejected bool
	// Slot is the event time whose interval must be recomputed.
	Slot clock.Ticks
}

// Offer evaluates a late event against the watermark. The caller first checks
// watermark.IsSealed; this method assumes the event is indeed late.
func (r *Replayer) Offer(ev *model.Event, mark clock.Ticks) Decision {
	age := mark - ev.EventTime
	if age > r.window {
		r.rejected++
		r.log.Add(model.MediaError{
			Category: model.CatLate,
			Stream:   ev.Stream,
			Seq:      ev.Seq,
			At:       ev.EventTime,
			Detail:   "late event beyond replay window; discarded",
			Decision: model.DecDropped,
		})
		return Decision{Rejected: true, Slot: ev.EventTime}
	}
	r.replays++
	r.log.Add(model.MediaError{
		Category: model.CatLate,
		Stream:   ev.Stream,
		Seq:      ev.Seq,
		At:       ev.EventTime,
		Detail:   "late event inside replay window; triggering deterministic replay",
		Decision: model.DecReplayed,
	})
	return Decision{Replay: true, Slot: ev.EventTime}
}

// Seal records an item that has left the pipeline as stable output. History is
// trimmed to both the replay window and the hard capacity bound.
func (r *Replayer) Seal(at clock.Ticks, stream uint32, seq uint16, hash uint64, order int) {
	r.history = append(r.history, SealedItem{At: at, Stream: stream, Seq: seq, Hash: hash, Order: order})
	cutoff := at - r.window
	drop := 0
	for drop < len(r.history) && r.history[drop].At < cutoff {
		drop++
	}
	if drop > 0 {
		r.history = append(r.history[:0], r.history[drop:]...)
	}
	if len(r.history) > r.maxHist {
		r.history = append(r.history[:0], r.history[len(r.history)-r.maxHist:]...)
	}
}

// Recompute returns the sealed items inside [slot, slot+window] in their
// original deterministic order. The pipeline re-runs recovery/sync over this
// exact set plus the new late event; because the set and order are fixed, the
// resulting hashes are reproducible across runs.
func (r *Replayer) Recompute(slot clock.Ticks) []SealedItem {
	out := make([]SealedItem, 0)
	for _, it := range r.history {
		if it.At >= slot && it.At <= slot+r.window {
			out = append(out, it)
		}
	}
	return out
}

// ReplayCount returns how many in-window late replays occurred.
func (r *Replayer) ReplayCount() int { return r.replays }

// RejectedCount returns how many late events were beyond the window.
func (r *Replayer) RejectedCount() int { return r.rejected }

// HistoryLen returns the current bounded history size.
func (r *Replayer) HistoryLen() int { return len(r.history) }
