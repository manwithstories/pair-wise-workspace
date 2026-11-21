// Package clock maintains the unified media timeline and adjudicates
// tfdt rollbacks without silently rewriting timestamps.
package clock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Traceable error kinds. Every irregularity is returned as an error AND
// recorded as an Issue; timestamps are never silently rewritten.
var (
	ErrSeqGap       = errors.New("clock: mfhd sequence gap")
	ErrDupSeq       = errors.New("clock: duplicate mfhd sequence number")
	ErrTFDTRollback = errors.New("clock: tfdt rollback")
	ErrTFDTOverflow = errors.New("clock: tfdt overflow")
)

// Frag is one parsed fragment placed on the unified timeline.
type Frag struct {
	Track       string
	Seq         uint32 // mfhd sequence number
	TFDT        uint64 // tfdt base media decode time (track timescale units)
	TFDTVersion int    // tfdt box version (0 => 32-bit field)
	Duration    uint64 // fragment duration in timescale units
	Timescale   uint32
	Index       int // index of the fragment within its track, in arrival order
}

// Issue is a recorded, locatable timeline irregularity.
type Issue struct {
	Kind   string // "seq-gap" | "dup-seq" | "tfdt-rollback" | "tfdt-overflow"
	Track  string
	Seq    uint32
	Detail string
}

type trackState struct {
	frags   []Frag
	seenSeq map[uint32]bool
	last    *Frag
}

// Timeline accumulates fragments per track on a shared millisecond base.
type Timeline struct {
	tracks    map[string]*trackState
	Issues    []Issue
	Rollbacks int
}

func New() *Timeline {
	return &Timeline{tracks: map[string]*trackState{}}
}

// Add places a fragment on the timeline. Irregularities produce a
// traceable error; the fragment is still recorded at its stated time.
func (t *Timeline) Add(f Frag) error {
	st := t.tracks[f.Track]
	if st == nil {
		st = &trackState{seenSeq: map[uint32]bool{}}
		t.tracks[f.Track] = st
	}
	if st.seenSeq[f.Seq] {
		t.Issues = append(t.Issues, Issue{Kind: "dup-seq", Track: f.Track, Seq: f.Seq,
			Detail: "mfhd sequence number already present on track"})
		return ErrDupSeq
	}
	st.seenSeq[f.Seq] = true

	// Overflow checks: 32-bit tfdt (version 0) must not wrap, and the
	// 64-bit addition itself must not overflow.
	end, carry := addUint64(f.TFDT, f.Duration)
	if carry || (f.TFDTVersion == 0 && end > math.MaxUint32) {
		t.Issues = append(t.Issues, Issue{Kind: "tfdt-overflow", Track: f.Track, Seq: f.Seq,
			Detail: fmt.Sprintf("tfdt %d + duration %d overflows %d-bit field", f.TFDT, f.Duration, 32*(2-f.TFDTVersion/2))})
		return ErrTFDTOverflow
	}

	var retErr error
	if st.last != nil {
		if f.Seq != st.last.Seq+1 {
			t.Issues = append(t.Issues, Issue{Kind: "seq-gap", Track: f.Track, Seq: f.Seq,
				Detail: fmt.Sprintf("expected seq %d, got %d (%d missing)", st.last.Seq+1, f.Seq, f.Seq-st.last.Seq-1)})
			retErr = ErrSeqGap
		}
		expected, _ := addUint64(st.last.TFDT, st.last.Duration)
		if f.TFDT < expected {
			t.Rollbacks++
			t.Issues = append(t.Issues, Issue{Kind: "tfdt-rollback", Track: f.Track, Seq: f.Seq,
				Detail: fmt.Sprintf("tfdt %d precedes expected %d; kept as stated", f.TFDT, expected)})
			retErr = ErrTFDTRollback
		}
	}
	cp := f
	st.last = &cp
	st.frags = append(st.frags, f)
	return retErr
}

func addUint64(a, b uint64) (uint64, bool) {
	s := a + b
	return s, s < a
}

// Frags returns the recorded fragments of a track ordered by tfdt.
func (t *Timeline) Frags(track string) []Frag {
	st := t.tracks[track]
	if st == nil {
		return nil
	}
	out := make([]Frag, len(st.frags))
	copy(out, st.frags)
	sort.Slice(out, func(i, j int) bool {
		if out[i].TFDT != out[j].TFDT {
			return out[i].TFDT < out[j].TFDT
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// Tracks returns sorted track names.
func (t *Timeline) Tracks() []string {
	names := make([]string, 0, len(t.tracks))
	for n := range t.tracks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ToMs converts timescale units to milliseconds on the unified base.
func ToMs(units uint64, timescale uint32) float64 {
	if timescale == 0 {
		return 0
	}
	return float64(units) * 1000.0 / float64(timescale)
}

// Summary is a deterministic digest of the timeline: identical input
// always yields an identical summary.
func (t *Timeline) Summary() string {
	h := sha256.New()
	for _, tr := range t.Tracks() {
		for _, f := range t.Frags(tr) {
			fmt.Fprintf(h, "%s/%d/%d/%d/%d\n", tr, f.Seq, f.TFDT, f.Duration, f.Timescale)
		}
	}
	fmt.Fprintf(h, "rollbacks=%d issues=%d\n", t.Rollbacks, len(t.Issues))
	return hex.EncodeToString(h.Sum(nil))[:16]
}
