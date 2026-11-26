package timeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"relay/internal/ingest"
	"relay/internal/media"
)

// RepairedFragment is one accepted fragment after timeline repair. It keeps
// the parsed fragment plus the renumbered contiguous sequence and the
// repaired decode time expressed in the shared global timescale.
type RepairedFragment struct {
	*ingest.Fragment
	NewSequence  uint32
	GlobalDTS    uint64
	RepairedTfdt uint64 // monotonic baseMediaDecodeTime in track timescale
	RawHash      string // sha256 of the fragment's source bytes
}

// TrackReport summarizes one track's repair outcome.
type TrackReport struct {
	TrackID    uint32
	Accepted   []*RepairedFragment
	Deduped    int
	GapsFilled []uint32
	Isolated   []uint32 // source sequences quarantined
	Fault      *media.Fault
}

// RepairTrack deduplicates, validates sequence numbers, fills mfhd gaps and
// repairs tfdt rollback for one track. A fault that cannot be handled
// losslessly isolates the whole track; its fragments never reach the index.
func RepairTrack(trackID uint32, clock media.Clock, frags []*ingest.Fragment) *TrackReport {
	rep := &TrackReport{TrackID: trackID}

	// 1. Group by source sequence, detecting duplicates and conflicts.
	bySeq := map[uint32][]*ingest.Fragment{}
	for _, f := range frags {
		bySeq[f.Sequence] = append(bySeq[f.Sequence], f)
	}
	seqs := make([]uint32, 0, len(bySeq))
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	chosen := make(map[uint32]*ingest.Fragment, len(seqs))
	hashes := make(map[uint32]string, len(seqs))
	for _, seq := range seqs {
		group := bySeq[seq]
		first := group[0]
		h1 := hashOf(first.Raw)
		for _, extra := range group[1:] {
			if hashOf(extra.Raw) == h1 {
				rep.Deduped++ // identical redelivery: keep one copy
				continue
			}
			rep.Fault = media.NewFault(media.FaultConflict, trackID, seq,
				extra.MoofOffset, "same sequence number with different content")
			rep.Isolated = append(rep.Isolated, seq)
			return rep
		}
		chosen[seq] = first
		hashes[seq] = h1
	}

	// 2. Report genuine mfhd gaps from adjacent source sequence numbers, then
	// renumber every accepted fragment into a gap-free 1..N space.
	for i := 1; i < len(seqs); i++ {
		for missing := seqs[i-1] + 1; missing < seqs[i]; missing++ {
			rep.GapsFilled = append(rep.GapsFilled, missing)
		}
	}
	nextSeq := uint32(1)
	ordered := make([]*RepairedFragment, 0, len(chosen))
	var prevEnd uint64 // expected next DTS in track timescale
	first := true
	for _, seq := range seqs {
		f := chosen[seq]
		dts := f.Tfdt
		if !first && dts < prevEnd {
			// tfdt rollback: advance to the previous fragment's end without
			// touching any per-sample composition offset.
			dts = prevEnd
		}
		// Sum sample durations and reject non-lossless overflow.
		var span uint64
		ok := true
		for _, s := range f.Samples {
			span, ok = media.AddTime(span, uint64(s.Duration))
			if !ok {
				rep.Fault = media.NewFault(media.FaultOverflow, trackID, seq,
					f.MoofOffset, "sample duration sum overflows")
				rep.Isolated = append(rep.Isolated, seq)
				return rep
			}
		}
		nextEnd, ok := media.AddTime(dts, span)
		if !ok {
			rep.Fault = media.NewFault(media.FaultUnrepairable, trackID, seq,
				f.MoofOffset, "repaired DTS plus duration overflows")
			rep.Isolated = append(rep.Isolated, seq)
			return rep
		}
		// Convert to the shared clock; an overflow isolates the track.
		gdts, ok := clock.ToGlobal(dts)
		if !ok {
			rep.Fault = media.NewFault(media.FaultOverflow, trackID, seq,
				f.MoofOffset, fmt.Sprintf("tfdt %d overflows global clock", dts))
			rep.Isolated = append(rep.Isolated, seq)
			return rep
		}
		ordered = append(ordered, &RepairedFragment{
			Fragment: f, NewSequence: nextSeq,
			GlobalDTS: gdts, RepairedTfdt: dts, RawHash: hashes[seq],
		})
		prevEnd = nextEnd
		nextSeq++
		first = false
	}

	// Monotonicity check on the repaired global DTS.
	for i := 1; i < len(ordered); i++ {
		if ordered[i].GlobalDTS < ordered[i-1].GlobalDTS {
			rep.Fault = media.NewFault(media.FaultUnrepairable, trackID,
				ordered[i].Sequence, ordered[i].MoofOffset,
				"global DTS not monotonic after repair")
			rep.Isolated = append(rep.Isolated, ordered[i].Sequence)
			return rep
		}
	}
	rep.Accepted = ordered
	return rep
}

// Key is the stable fragment identifier used by checkpoints and outputs.
func (r *RepairedFragment) Key() string {
	return fmt.Sprintf("t%03d/f%05d.m4s", r.TrackID, r.NewSequence)
}

func hashOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
