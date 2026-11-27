// Package recovery turns per-track sequence holes into either interpolated
// frames or explicit missing-frame records. It never reports a hole as
// recovered unless both neighbours are valid and lie within two frames.
package recovery

import (
	"errors"
	"fmt"
	"sort"

	"fieldcut/internal/ingest"
	"fieldcut/internal/media"
)

// MaxGapFrames is the largest hole that may be interpolated.
const MaxGapFrames = 2

// Slot is one sequence position on a track.
type Slot struct {
	Seq     int64
	Present bool
	Valid   bool // present and decoded/verified successfully
	Err     *media.MediaError
}

// Interp describes a synthesized frame between two valid neighbours.
type Interp struct {
	Seq    int64
	Track  string
	Before int64          // seq of the earlier valid frame
	After  int64          // seq of the later valid frame
	Weight media.Rational // position between Before and After
}

// Missing is an explicit record for a hole that was not recovered.
type Missing struct {
	Track  string
	Seq    int64
	Reason string
}

// Plan is the recovery outcome for one track.
type Plan struct {
	Track           string
	Slots           []Slot
	Interps         []Interp
	Missing         []Missing
	Eligible        int // recoverable injected missing frames (the rate denominator)
	Recovered       int // injected holes actually synthesized
	CorruptRepaired int // corrupt frames rebuilt from valid neighbours
}

// Rate is recovered/eligible. With nothing eligible the track is whole, so
// the rate is 1.0. Corrupt repairs never enter the numerator or denominator:
// the denominator counts only injected missing frames.
func (p Plan) Rate() float64 {
	if p.Eligible == 0 {
		return 1.0
	}
	return float64(p.Recovered) / float64(p.Eligible)
}

// Dedupe sorts entries by sequence and keeps the first arrival of each
// sequence, matching the ingest rule for out-of-order deliveries.
func Dedupe(entries []ingest.Entry) []ingest.Entry {
	seen := make(map[int64]bool, len(entries))
	out := make([]ingest.Entry, 0, len(entries))
	for _, e := range entries {
		if seen[e.Seq] {
			continue
		}
		seen[e.Seq] = true
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	return out
}

// Analyze builds the recovery plan for a track of declaredFrames positions.
// present is the deduplicated entry set; valid marks frames that decoded
// successfully. Corrupt frames stay present but are only rebuilt when a
// valid neighbour on each side exists; they never count as recovered holes.
func Analyze(track string, declaredFrames int, present []ingest.Entry, valid func(ingest.Entry) bool) (Plan, error) {
	if declaredFrames <= 0 {
		return Plan{}, media.Newf(media.KindInput, "recovery.analyze", track, -1, "declared frames %d", declaredFrames)
	}
	if valid == nil {
		return Plan{}, media.New(media.KindInput, "recovery.analyze", track, -1, errors.New("nil validity function"))
	}
	entries := Dedupe(present)
	bySeq := make(map[int64]ingest.Entry, len(entries))
	for _, e := range entries {
		if e.Seq >= int64(declaredFrames) {
			return Plan{}, media.Newf(media.KindInput, "recovery.analyze", track, e.Seq,
				"sequence beyond declared %d", declaredFrames)
		}
		if e.Track != track {
			return Plan{}, media.Newf(media.KindInput, "recovery.analyze", e.Track, e.Seq, "entry from another track")
		}
		bySeq[e.Seq] = e
	}
	plan := Plan{Track: track, Slots: make([]Slot, 0, declaredFrames)}
	state := make(map[int64]Slot, declaredFrames)
	for i := 0; i < declaredFrames; i++ {
		e, ok := bySeq[int64(i)]
		s := Slot{Seq: int64(i), Present: ok}
		if ok {
			s.Valid = valid(e)
			if !s.Valid {
				s.Err = media.Newf(media.KindCorrupt, "recovery.decode", track, int64(i), "frame failed to decode")
			}
		}
		state[int64(i)] = s
	}
	// Walk invalid runs (absent or corrupt). Interpolation is allowed only
	// with a valid flank on both sides and at most MaxGapFrames positions.
	for i := 0; i < declaredFrames; {
		if state[int64(i)].Valid {
			i++
			continue
		}
		j := i
		for j < declaredFrames && !state[int64(j)].Valid {
			j++
		}
		runLen := j - i
		hasBefore := i-1 >= 0 && state[int64(i-1)].Valid
		hasAfter := j < declaredFrames && state[int64(j)].Valid
		if hasBefore && hasAfter && runLen <= MaxGapFrames {
			span := int64(runLen + 1)
			for k := int64(1); k <= int64(runLen); k++ {
				seq := int64(i) + k - 1
				plan.Interps = append(plan.Interps, Interp{
					Seq:    seq,
					Track:  track,
					Before: int64(i) - 1,
					After:  int64(j),
					Weight: media.NewRational(k, span),
				})
				if state[seq].Present {
					plan.CorruptRepaired++
				} else {
					plan.Eligible++
					plan.Recovered++
				}
				s := state[seq]
				s.Valid = true
				s.Err = nil
				state[seq] = s
			}
		} else {
			for k := i; k < j; k++ {
				seq := int64(k)
				reason := "hole"
				switch {
				case state[seq].Present:
					reason = "corrupt"
				case !hasBefore:
					reason = "boundary-before"
				case !hasAfter:
					reason = "boundary-after"
				default:
					reason = "gap-too-long"
				}
				plan.Missing = append(plan.Missing, Missing{Track: track, Seq: seq, Reason: reason})
			}
		}
		i = j
	}
	for i := 0; i < declaredFrames; i++ {
		plan.Slots = append(plan.Slots, state[int64(i)])
	}
	return plan, nil
}

// FrameAt returns the slot and its state for a sequence position.
func (p Plan) FrameAt(seq int64) (Slot, bool) {
	if seq < 0 || seq >= int64(len(p.Slots)) {
		return Slot{}, false
	}
	return p.Slots[seq], true
}

// InterpAt returns the interpolation record covering seq, if any.
func (p Plan) InterpAt(seq int64) (Interp, bool) {
	for _, in := range p.Interps {
		if in.Seq == seq {
			return in, true
		}
	}
	return Interp{}, false
}

func (p Plan) String() string {
	return fmt.Sprintf("track=%s eligible=%d recovered=%d corrupt_repaired=%d missing=%d rate=%.4f",
		p.Track, p.Eligible, p.Recovered, p.CorruptRepaired, len(p.Missing), p.Rate())
}
