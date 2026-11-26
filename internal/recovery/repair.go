package recovery

import (
	"sort"

	"mediaqc/internal/jitter"
	"mediaqc/internal/media"
	rtpio "mediaqc/internal/rtp"
)

// DefaultMaxCopies bounds the retransmission cache.
const DefaultMaxCopies = 256

// Frame is one timeline position after repair. Gap frames carry no audio.
type Frame struct {
	ExtSeq    uint64
	PTS       uint64
	Payload   []byte
	Source    string
	Repaired  bool
	Gap       bool
	ArrivalMS int64
}

// Report contains the deterministic repaired timeline and recovery metrics.
type Report struct {
	Frames           []Frame `json:"-"`
	Recovered        int     `json:"recovered"`
	RecoverableLoss  int     `json:"recoverable_loss"`
	UnrecoverableGap int     `json:"unrecoverable_gaps"`
	RecoveryRate     float64 `json:"recovery_rate"`
}

// Repairer stores retransmission copies and fills explicit jitter gaps.
type Repairer struct {
	copies    map[uint64]*rtpio.Packet
	maxCopies int
}

// NewRepairer creates a bounded retransmission cache.
func NewRepairer(maxCopies int) *Repairer {
	if maxCopies <= 0 {
		maxCopies = DefaultMaxCopies
	}
	return &Repairer{copies: make(map[uint64]*rtpio.Packet), maxCopies: maxCopies}
}

// AddRetransmission stores a valid repair copy keyed by extended sequence.
func (r *Repairer) AddRetransmission(p *rtpio.Packet) error {
	if p == nil {
		return media.New(media.KindMalformed, "recovery.AddRetransmission", "nil packet")
	}
	if _, exists := r.copies[p.ExtSequence]; !exists && len(r.copies) >= r.maxCopies {
		return media.New(media.KindInternal, "recovery.AddRetransmission", "retransmission cache full")
	}
	r.copies[p.ExtSequence] = p
	return nil
}

// HasCopy reports whether a sequence has a cached retransmission copy.
func (r *Repairer) HasCopy(extSeq uint64) bool {
	_, ok := r.copies[extSeq]
	return ok
}

// Repair merges ordered packets, jitter gaps and retransmission copies.
func (r *Repairer) Repair(ordered []*rtpio.Packet, gaps []jitter.Gap) *Report {
	timeline := make(map[uint64]Frame, len(ordered))
	for _, p := range ordered {
		timeline[p.ExtSequence] = Frame{
			ExtSeq: p.ExtSequence, PTS: p.ExtTimestamp,
			Payload: append([]byte(nil), p.Payload...), Source: "original",
			ArrivalMS: p.ArrivalMS,
		}
	}

	rep := &Report{}
	for _, gap := range gaps {
		for _, seq := range gap.Missing {
			if copyPkt, ok := r.copies[seq]; ok && validCopy(copyPkt) {
				timeline[seq] = Frame{
					ExtSeq: seq, PTS: copyPkt.ExtTimestamp,
					Payload: append([]byte(nil), copyPkt.Payload...), Source: "retransmission",
					Repaired: true, ArrivalMS: copyPkt.ArrivalMS,
				}
				rep.Recovered++
				rep.RecoverableLoss++
				continue
			}

			// Explicit gap only: no synthetic audio payload is created.
			timeline[seq] = Frame{
				ExtSeq: seq, Gap: true, Source: "gap",
				PTS: ptsForMissing(seq, timeline, gaps),
			}
			rep.UnrecoverableGap++
		}
	}

	seqs := make([]uint64, 0, len(timeline))
	for seq := range timeline {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	rep.Frames = make([]Frame, 0, len(seqs))
	for _, seq := range seqs {
		rep.Frames = append(rep.Frames, timeline[seq])
	}

	if rep.RecoverableLoss > 0 {
		rep.RecoveryRate = float64(rep.Recovered) / float64(rep.RecoverableLoss)
	} else {
		rep.RecoveryRate = 1
	}
	return rep
}

func validCopy(p *rtpio.Packet) bool {
	return p != nil && len(p.Payload) > 0
}

// ptsForMissing uses a neighboring timestamp when available; callers generate
// fixed-duration media, so pipeline normalization can also assign PTS later.
func ptsForMissing(seq uint64, timeline map[uint64]Frame, gaps []jitter.Gap) uint64 {
	if f, ok := timeline[seq+1]; ok && f.PTS > 0 {
		// The exact duration is applied by the synchronizer/codec descriptor.
		return f.PTS
	}
	if f, ok := timeline[seq-1]; ok && f.PTS > 0 {
		return f.PTS
	}
	return uint64(seq) * 1800
}
