// Package stream analyzes binary media streams: MP4 box structure with
// keyframe intervals and RTP sequence continuity. Both analyzers report
// damage through the unified fault model; RTP network loss is never recorded
// as a file byte offset.
package stream

import (
	"encoding/binary"
	"io"

	"github.com/pion/rtp"

	"mediascan/internal/fault"
)

// RTPGap describes one missing sequence-number span.
type RTPGap struct {
	From uint16 // first missing sequence number
	To   uint16 // last missing sequence number (inclusive)
	Lost int    // number of missing packets
}

// RTPStats is the RTP analysis result for one source.
type RTPStats struct {
	Source      string
	Packets     int // unique packets accepted
	Duplicates  int // repeated sequence numbers still inside the window
	Gaps        []RTPGap
	OutOfWindow int // late packets that could no longer be reordered
	LateSeqs    []uint16
	MaxSeq      uint16
	SSRC        uint32
	Faults      []fault.Fault
}

// DefaultReorderWindow is the maximum out-of-order distance (in packets)
// that can be absorbed, matching typical real-time RTP jitter buffers.
const DefaultReorderWindow = 64

// reorderWindow is a sliding view over 16-bit sequence numbers. base is one
// past the oldest confirmed/retired number; lead is the highest admitted.
// seen holds arrivals whose number lies in (base-W-1, lead].
type reorderWindow struct {
	seen map[uint16]bool
	base uint16 // next number expected at the trailing edge
	lead uint16 // highest sequence number ever admitted
	have bool
	size uint16
}

func newReorderWindow() *reorderWindow {
	return &reorderWindow{seen: make(map[uint16]bool, DefaultReorderWindow+2), size: DefaultReorderWindow}
}

// admit classifies a sequence number and returns any hole spans that become
// confirmed by this admission.
func (w *reorderWindow) admit(seq uint16) (kind string, retired []RTPGap) {
	if !w.have {
		w.have = true
		w.base, w.lead = seq, seq
		w.seen[seq] = true
		return "ok", nil
	}
	// Duplicate: any already-admitted number that is still retained.
	if w.seen[seq] {
		return "dup", nil
	}
	// Late: an aged-out number strictly behind base. Numbers in [base,
	// lead) are open holes that in-window reordering can still fill.
	if w.older(seq, w.base) {
		return "late", nil
	}
	// Forward jump larger than the window retires every open hole. The
	// direction check excludes backward hole-filling packets, whose
	// unsigned seq-lead difference would otherwise wrap to a huge number.
	if w.older(w.lead, seq) && seq-w.lead > w.size {
		retired = w.holesBetween(w.lead+1, seq)
		w.seen = map[uint16]bool{seq: true}
		w.base, w.lead = seq, seq
		return "gap", retired
	}
	// Regular admission (in-window fill or forward step).
	w.seen[seq] = true
	if w.older(w.lead, seq) {
		w.lead = seq
	}
	retired = w.advanceBase()
	return "ok", retired
}

// older reports whether a precedes b in unsigned 16-bit forward order.
func (w *reorderWindow) older(a, b uint16) bool { return b-a > 0 && b-a < 1<<15 }

// advanceBase enforces the window bound while keeping base pinned at the
// oldest unconfirmed slot. Present packets that age out are simply evicted;
// an absent slot that ages out is a confirmed gap and only then moves base.
func (w *reorderWindow) advanceBase() []RTPGap {
	var retired []RTPGap
	// Evict delivered packets trailing the window without moving base past
	// any open hole.
	for w.seen[w.base] && w.older(w.base, w.lead) && w.lead-w.base > w.size {
		delete(w.seen, w.base)
		w.base++
	}
	// Retire contiguous aged-out holes once they leave the window.
	for w.older(w.base, w.lead) && w.lead-w.base > w.size {
		from := w.base
		for w.older(w.base, w.lead) && w.lead-w.base > w.size && !w.seen[w.base] {
			w.base++
		}
		retired = append(retired, RTPGap{From: from, To: w.base - 1, Lost: int(w.base - from)})
	}
	return retired
}

// holesBetween lists absent numbers in [from, to) as merged spans.
func (w *reorderWindow) holesBetween(from, to uint16) []RTPGap {
	var gaps []RTPGap
	for seq := from; seq != to; seq++ {
		if w.seen[seq] {
			continue
		}
		if n := len(gaps); n > 0 && gaps[n-1].To+1 == seq {
			gaps[n-1].To = seq
			gaps[n-1].Lost++
		} else {
			gaps = append(gaps, RTPGap{From: seq, To: seq, Lost: 1})
		}
	}
	return gaps
}

// drain reports holes still open at stream end that have aged out of the
// window. Holes within size of lead could still have been in flight. The
// loop stops as soon as base reaches lead: nothing newer can be a hole.
func (w *reorderWindow) drain() []RTPGap {
	if !w.have {
		return nil
	}
	var gaps []RTPGap
	for seq := w.base; w.older(seq, w.lead) && w.lead-seq > w.size; seq++ {
		if w.seen[seq] {
			continue
		}
		if n := len(gaps); n > 0 && gaps[n-1].To+1 == seq {
			gaps[n-1].To = seq
			gaps[n-1].Lost++
		} else {
			gaps = append(gaps, RTPGap{From: seq, To: seq, Lost: 1})
		}
	}
	return gaps
}

// AnalyzeRTP reads length-prefixed RTP packets from r. Each record on disk is
// a 2-byte big-endian length followed by that many RTP bytes — the framing is
// external (our own capture format), so packet loss is tracked purely by
// sequence number and never mislabeled as a file offset.
func AnalyzeRTP(r io.Reader, source string) (*RTPStats, error) {
	stats := &RTPStats{Source: source}
	win := newReorderWindow()
	var filePos int64

	for {
		var lenBuf [2]byte
		n, err := io.ReadFull(r, lenBuf[:])
		if err == io.EOF && n == 0 {
			break
		}
		if err == io.ErrUnexpectedEOF {
			stats.Faults = append(stats.Faults, fault.New(source, filePos, fault.StageRTP, false,
				"truncated RTP length prefix, %d of 2 bytes", n))
			break
		}
		if err != nil {
			return nil, err
		}
		recLen := int(binary.BigEndian.Uint16(lenBuf[:]))
		recStart := filePos
		filePos += 2
		if int64(recLen) > fault.MaxInputSize {
			return nil, &fault.Fault{Source: source, Offset: recStart, Stage: fault.StageRTP,
				Recoverable: false, Message: "RTP record length exceeds 2 GiB limit"}
		}
		buf, err := fault.SafeAlloc(int64(recLen))
		if err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			stats.Faults = append(stats.Faults, fault.New(source, recStart, fault.StageRTP, true,
				"truncated RTP packet: want %d bytes: %v", recLen, err))
			break
		}
		filePos += int64(recLen)

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf); err != nil {
			stats.Faults = append(stats.Faults, fault.New(source, recStart, fault.StageRTP, true,
				"unparseable RTP packet: %v", err))
			continue
		}
		if stats.Packets == 0 && stats.Duplicates == 0 && stats.OutOfWindow == 0 {
			stats.SSRC = pkt.SSRC
		}
		kind, retired := win.admit(pkt.SequenceNumber)
		for _, g := range retired {
			stats.addGap(g)
		}
		switch kind {
		case "dup":
			stats.Duplicates++
		case "late":
			stats.OutOfWindow++
			stats.LateSeqs = append(stats.LateSeqs, pkt.SequenceNumber)
			stats.Faults = append(stats.Faults, fault.New(source, -1, fault.StageRTP, true,
				"RTP packet %d arrived outside reorder window", pkt.SequenceNumber))
		case "ok", "gap":
			stats.Packets++
			stats.MaxSeq = win.lead
		}
	}
	for _, g := range win.drain() {
		stats.addGap(g)
	}
	return stats, nil
}

// addGap records a loss span and its offset-free Fault; adjacent spans merge.
func (s *RTPStats) addGap(g RTPGap) {
	if n := len(s.Gaps); n > 0 && s.Gaps[n-1].To+1 == g.From {
		s.Gaps[n-1].To = g.To
		s.Gaps[n-1].Lost += g.Lost
	} else {
		s.Gaps = append(s.Gaps, g)
	}
	s.Faults = append(s.Faults, fault.New(s.Source, -1, fault.StageRTP, true,
		"RTP sequence gap %d-%d (%d packets lost)", g.From, g.To, g.Lost))
}
