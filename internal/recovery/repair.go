// Package recovery classifies incoming packets against the expected
// sequence and tracks which gaps can still be filled.
package recovery

import (
	"fmt"
	"sort"

	"mediad/internal/media"
)

// Class is the recovery classification of one observed packet.
type Class int

const (
	// ClassNew is a packet arriving at or beyond the expected sequence.
	ClassNew Class = iota
	// ClassDuplicate is a packet already accounted for.
	ClassDuplicate
	// ClassGapFill is a late packet filling a previously recorded gap.
	ClassGapFill
)

// Stats accumulates per-track recovery counters.
type Stats struct {
	Duplicates    int64
	GapFills      int64
	Lost          int64 // currently missing, fillable
	Unrecoverable int64 // gaps declared unrecoverable
}

// Tracker follows one track's extended sequence numbers.
type Tracker struct {
	track    uint32
	maxFill  int
	started  bool
	expected uint64
	missing  map[uint64]bool
	stats    Stats
}

// New creates a tracker. Gaps longer than maxFill packets are declared
// unrecoverable as soon as they are observed.
func New(track uint32, maxFill int) *Tracker {
	if maxFill <= 0 {
		maxFill = 16
	}
	return &Tracker{track: track, maxFill: maxFill, missing: make(map[uint64]bool)}
}

func (t *Tracker) Stats() Stats { return t.stats }

// SetStats replaces the counters, e.g. after a checkpoint load.
func (t *Tracker) SetStats(s Stats) { t.stats = s }

// Expected returns the next unseen sequence number.
func (t *Tracker) Expected() uint64 { return t.expected }

// Missing returns the still-fillable gap sequences, sorted.
func (t *Tracker) Missing() []uint64 {
	out := make([]uint64, 0, len(t.missing))
	for s := range t.missing {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Restore replaces the tracker state, e.g. after a checkpoint load.
func (t *Tracker) Restore(expected uint64, missing []uint64) {
	t.started = true
	t.expected = expected
	t.missing = make(map[uint64]bool, len(missing))
	for _, s := range missing {
		t.missing[s] = true
	}
}

// Observe classifies seq. Anomalies are reported as faults; duplicates
// and gap fills never move the expected pointer.
func (t *Tracker) Observe(seq uint64, at int64) (Class, *media.Fault) {
	if !t.started {
		t.started = true
		t.expected = seq + 1
		return ClassNew, nil
	}
	switch {
	case seq >= t.expected:
		if seq == t.expected {
			t.expected++
			return ClassNew, nil
		}
		start := t.expected
		gap := seq - start
		t.expected = seq + 1
		if int(gap) <= t.maxFill {
			for s := start; s < seq; s++ {
				t.missing[s] = true
			}
			t.stats.Lost += int64(gap)
			return ClassNew, &media.Fault{
				Kind: media.FaultLoss, Op: "recovery.repair",
				Track: t.track, Seq: start, At: at, Count: int64(gap),
				Msg: fmt.Sprintf("fillable gap of %d packet(s) starting at %d", gap, start),
			}
		}
		t.stats.Unrecoverable += int64(gap)
		return ClassNew, &media.Fault{
			Kind: media.FaultUnrecoverable, Op: "recovery.repair",
			Track: t.track, Seq: start, At: at, Count: int64(gap),
			Msg: fmt.Sprintf("unrecoverable gap of %d packet(s) starting at %d", gap, start),
		}
	default: // seq < expected
		if t.missing[seq] {
			delete(t.missing, seq)
			t.stats.GapFills++
			t.stats.Lost--
			return ClassGapFill, &media.Fault{
				Kind: media.FaultReorder, Op: "recovery.repair",
				Track: t.track, Seq: seq, At: at,
				Msg: "late packet filled a recorded gap",
			}
		}
		t.stats.Duplicates++
		return ClassDuplicate, &media.Fault{
			Kind: media.FaultDuplicate, Op: "recovery.repair",
			Track: t.track, Seq: seq, At: at,
			Msg: "duplicate packet",
		}
	}
}

// Finalize declares every still-open gap unrecoverable and returns the
// lost sequences, sorted. Called at end of stream.
func (t *Tracker) Finalize() []uint64 {
	left := t.Missing()
	t.stats.Unrecoverable += int64(len(left))
	t.stats.Lost -= int64(len(left))
	t.missing = make(map[uint64]bool)
	return left
}

// TimedItem couples a sequence number with its media time.
type TimedItem struct {
	Seq  uint64
	Time media.MediaTime
}

// SortByMediaTime orders repaired items by media time, breaking ties by
// sequence number so the result is deterministic.
func SortByMediaTime(items []TimedItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if c := media.Compare(items[i].Time, items[j].Time); c != 0 {
			return c < 0
		}
		return items[i].Seq < items[j].Seq
	})
}
