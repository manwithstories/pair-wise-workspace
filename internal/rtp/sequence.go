// Package rtp validates RTP sequence numbers and timestamp continuity on top
// of the clock package's wrap unwrapping, and translates findings into the
// shared fault model.
package rtp

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

// Limits bounds accepted RTP streams to prevent unbounded memory use.
type Limits struct {
	MaxPackets  int
	MaxTracks   int
	MaxQueueLen int
}

// DefaultLimits returns conservative local caps.
func DefaultLimits() Limits {
	return Limits{MaxPackets: 1 << 22, MaxTracks: 32, MaxQueueLen: 4096}
}

// Track is one RTP stream's packets in arrival order.
type Track struct {
	TrackID   uint32
	ClockRate int64
	Packets   []*rtp.Packet
}

// SeqReport describes sequence-number continuity for one track.
type SeqReport struct {
	TrackID    uint32
	Total      int
	Duplicates int
	// Gaps lists each missing run: the first and last (exclusive) extended
	// sequence numbers that were never observed.
	Gaps []SeqGap
}

// SeqGap is a missing run of extended sequence numbers [Start, End).
type SeqGap struct {
	Start int64
	End   int64
}

// MissingCount sums the packets absent from the stream.
func (r SeqReport) MissingCount() int64 {
	var n int64
	for _, g := range r.Gaps {
		n += g.End - g.Start
	}
	return n
}

// ErrEmptyTrack is returned when a track carries no packets.
var ErrEmptyTrack = errors.New("rtp: empty track")

// ErrLimit is returned when a configured cap is exceeded.
var ErrLimit = errors.New("rtp: limit exceeded")

// AnalyzeSequence extends 16-bit sequence numbers and detects duplicates and
// missing runs. Out-of-order delivery is tolerated and does not by itself
// produce a fault; only genuinely absent sequence numbers do.
func AnalyzeSequence(t Track, lim Limits) (SeqReport, error) {
	rep := SeqReport{TrackID: t.TrackID}
	if len(t.Packets) == 0 {
		return rep, ErrEmptyTrack
	}
	if len(t.Packets) > lim.MaxPackets {
		return rep, fmt.Errorf("%w: %d packets", ErrLimit, len(t.Packets))
	}
	seen := make(map[int64]bool, len(t.Packets))
	var ext int64
	var minSeq, maxSeq int64
	for i, p := range t.Packets {
		if i > 0 {
			delta := int16(p.SequenceNumber - t.Packets[i-1].SequenceNumber)
			ext += int64(delta)
		} else {
			ext = int64(p.SequenceNumber)
			minSeq, maxSeq = ext, ext
		}
		if seen[ext] {
			rep.Duplicates++
		}
		seen[ext] = true
		if ext < minSeq {
			minSeq = ext
		}
		if ext > maxSeq {
			maxSeq = ext
		}
	}
	rep.Total = len(t.Packets)
	// Walk the full extended range and group absent numbers into runs.
	for s := minSeq; s <= maxSeq; {
		if seen[s] {
			s++
			continue
		}
		start := s
		for s <= maxSeq && !seen[s] {
			s++
		}
		rep.Gaps = append(rep.Gaps, SeqGap{Start: start, End: s})
	}
	return rep, nil
}

// TimingReport combines wrap resolution with fault translation.
type TimingReport struct {
	TrackID     uint32
	Unwrapped   *clock.UnwrapResult
	WrapCount   int
	Ambiguities []clock.AmbiguitySpan
}

// AnalyzeTiming runs timestamp unwrapping and counts genuine 32-bit wraps by
// detecting raw timestamp decreases in send order (a wrap signature that is
// distinct from reordering, which never changes send-order adjacency).
func AnalyzeTiming(t Track) (*TimingReport, []fault.Fault, error) {
	if len(t.Packets) == 0 {
		return nil, nil, ErrEmptyTrack
	}
	pkts := make([]clock.Packet, len(t.Packets))
	for i, p := range t.Packets {
		pkts[i] = clock.Packet{Seq: p.SequenceNumber, TS: p.Timestamp}
	}
	res, err := clock.UnwrapTimestamps(pkts, clock.UnwrapConfig{ClockRate: t.ClockRate})
	if err != nil {
		return nil, nil, err
	}
	rep := &TimingReport{
		TrackID:     t.TrackID,
		Unwrapped:   res,
		Ambiguities: res.Ambiguities,
	}
	// Count wraps from send-order adjacent raw timestamp decreases.
	sendOrder := make([]int, len(pkts))
	for i := range sendOrder {
		sendOrder[i] = i
	}
	// Stable sort by extended sequence (already computed inside clock, but
	// derive ordering independently here from arrival ext sequence).
	var ext int64
	extByArrival := make([]int64, len(pkts))
	for i, p := range t.Packets {
		if i > 0 {
			ext += int64(int16(p.SequenceNumber - t.Packets[i-1].SequenceNumber))
		} else {
			ext = int64(p.SequenceNumber)
		}
		extByArrival[i] = ext
	}
	// Simple insertion sort is adequate and keeps ordering deterministic.
	for i := 1; i < len(sendOrder); i++ {
		for j := i; j > 0 && extByArrival[sendOrder[j]] < extByArrival[sendOrder[j-1]]; j-- {
			sendOrder[j], sendOrder[j-1] = sendOrder[j-1], sendOrder[j]
		}
	}
	var faults []fault.Fault
	for k := 1; k < len(sendOrder); k++ {
		prev := t.Packets[sendOrder[k-1]]
		cur := t.Packets[sendOrder[k]]
		if cur.Timestamp < prev.Timestamp {
			rep.WrapCount++
			idx := sendOrder[k]
			faults = append(faults, fault.New(
				fault.KindRTPWrap, fault.SeverityInfo, t.TrackID,
				int64(idx), -1, res.Packets[idx].Ticks,
				fmt.Sprintf("32-bit timestamp wrap between seq %d and %d",
					prev.SequenceNumber, cur.SequenceNumber),
			))
		}
	}
	for _, span := range res.Ambiguities {
		faults = append(faults, fault.New(
			fault.KindAmbiguous, fault.SeverityWarning, t.TrackID,
			int64(span.ArrivalStart), -1, 0,
			fmt.Sprintf("ambiguous timestamp jump at seq %d..%d",
				span.SeqStart, span.SeqEnd),
		))
	}
	return rep, faults, nil
}

// SequenceFaults translates gap/duplicate findings into the fault model.
func SequenceFaults(t Track, rep SeqReport) []fault.Fault {
	var out []fault.Fault
	for _, g := range rep.Gaps {
		out = append(out, fault.New(
			fault.KindRTPSeqGap, fault.SeverityError, t.TrackID,
			g.Start, -1, 0,
			fmt.Sprintf("missing sequence numbers [%d,%d)", g.Start, g.End),
		).WithSpan(g.End-g.Start, 0))
	}
	return out
}
