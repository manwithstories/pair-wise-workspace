package stream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pion/rtp"

	"mediascan/internal/model"
)

// RTP analysis limits.
const (
	MaxCaptureBytes = 32 << 20
	MaxRTPRecords   = 500_000
	// ReorderWindow bounds how far out of order a packet may arrive and still
	// be reordered instead of counted as unrecovered loss.
	ReorderWindow = 64
)

// RTPStats summarizes one capture.
type RTPStats struct {
	Packets      int            `json:"packets"`
	Expected     int            `json:"expected"`
	Dropped      int            `json:"packets_lost"`
	Duplicates   int            `json:"duplicates"`
	Reordered    int            `json:"reordered"`
	Recovered    int            `json:"recovered"`
	RecoveryPct  float64        `json:"loss_recovery_pct"`
	FirstLostSeq int            `json:"first_lost_seq"`
	TimestampGap bool           `json:"timestamp_anomaly"`
	Faults       []model.Fault  `json:"faults,omitempty"`
	LostRanges   []RTPLostRange `json:"lost_ranges,omitempty"`
}

// RTPLostRange records one contiguous sequence gap.
type RTPLostRange struct {
	StartSeq int `json:"start_seq"`
	EndSeq   int `json:"end_seq"`
}

// ReadCapture loads a length-prefixed capture of raw RTP packets. The parser
// streams from disk and holds at most one record at a time.
func ReadCapture(path string) ([]*rtp.Packet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxCaptureBytes {
		return nil, fmt.Errorf("%w: capture %d bytes", model.ErrBoxTooLarge, info.Size())
	}
	var out []*rtp.Packet
	for count := 0; ; count++ {
		if count > MaxRTPRecords {
			return nil, fmt.Errorf("%w: too many RTP records", model.ErrBoxTooLarge)
		}
		var hdr [4]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, trimErr(err)
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n < 12 || n > MaxCaptureBytes {
			return nil, fmt.Errorf("%w: rtp record length %d", model.ErrBoxBounds, n)
		}
		raw := make([]byte, n)
		if _, err := io.ReadFull(f, raw); err != nil {
			return nil, trimErr(err)
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			return nil, fmt.Errorf("rtp: record %d: %w", count, err)
		}
		out = append(out, pkt)
	}
	return out, nil
}

func trimErr(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: capture ends mid-record", model.ErrTruncated)
	}
	return err
}

// AnalyzeRTP replays a capture through a bounded reorder window. Packets that
// arrive out of order inside the window are restored to sequence order; a gap
// that survives the window (or the end of the capture) is an unrecovered loss.
//
// Duplicates are counted but never inflate the recovery rate: recovery is the
// share of distinct sequence numbers that were eventually delivered. A
// retransmission that arrives once fills a real gap and does count; a second
// copy of an already-delivered packet does not.
func AnalyzeRTP(pkts []*rtp.Packet) RTPStats {
	stats := RTPStats{Packets: len(pkts)}
	if len(pkts) == 0 {
		return stats
	}
	first, last := pkts[0].SequenceNumber, pkts[0].SequenceNumber
	for _, p := range pkts[1:] {
		if seqDiff(p.SequenceNumber, first) < 0 {
			first = p.SequenceNumber
		}
		if seqDiff(p.SequenceNumber, last) > 0 {
			last = p.SequenceNumber
		}
	}
	expected := seqDiff(last, first) + 1
	stats.Expected = expected
	if expected <= 0 || expected > MaxRTPRecords {
		stats.Faults = append(stats.Faults, model.Fault{
			Kind: model.FaultPacketLoss, Severity: model.SeverityCritical,
			Message: fmt.Sprintf("sequence span %d out of range", expected),
		})
		return stats
	}
	delivered := make(map[uint16]bool, len(pkts))
	seenDup := make(map[uint16]bool, len(pkts))
	// Reordering is measured against the highest sequence number seen so far.
	high := first
	for _, p := range pkts {
		seq := p.SequenceNumber
		if delivered[seq] || seenDup[seq] {
			stats.Duplicates++
			seenDup[seq] = true
			continue
		}
		if seqDiff(seq, high) < 0 {
			// Arrived after a higher sequence number: reordered if the gap is
			// still inside the window, otherwise a late arrival we cannot use.
			if -seqDiff(seq, high) <= ReorderWindow {
				stats.Reordered++
				delivered[seq] = true
				continue
			}
			// Outside the window: it is a duplicate for rate purposes; the
			// sequence slot stays missing.
			stats.Duplicates++
			seenDup[seq] = true
			continue
		}
		delivered[seq] = true
		if seqDiff(seq, high) > 0 {
			high = seq
		}
	}
	var lost, recovered int
	var ranges []RTPLostRange
	var open *RTPLostRange
	for i := 0; i < expected; i++ {
		seq := first + uint16(i)
		if delivered[seq] {
			recovered++
			if open != nil {
				ranges = append(ranges, *open)
				open = nil
			}
			continue
		}
		if stats.FirstLostSeq == 0 {
			stats.FirstLostSeq = int(seq)
		}
		lost++
		if open == nil {
			open = &RTPLostRange{StartSeq: int(seq), EndSeq: int(seq)}
		} else {
			open.EndSeq = int(seq)
		}
	}
	if open != nil {
		ranges = append(ranges, *open)
	}
	stats.Recovered = recovered
	stats.Dropped = lost
	stats.LostRanges = ranges
	if expected > 0 {
		stats.RecoveryPct = float64(recovered) / float64(expected) * 100
	}
	if lost > 0 {
		stats.Faults = append(stats.Faults, model.Fault{
			Kind: model.FaultPacketLoss, Severity: model.SeverityCritical,
			Message: fmt.Sprintf("%d of %d sequence numbers missing after reorder window", lost, expected),
		})
	}
	if stats.Reordered > 0 {
		stats.Faults = append(stats.Faults, model.Fault{
			Kind: model.FaultPacketLoss, Severity: model.SeverityWarning,
			Message: fmt.Sprintf("%d packets reordered inside the %d-packet window", stats.Reordered, ReorderWindow),
		})
	}
	if stats.Duplicates > 0 {
		stats.Faults = append(stats.Faults, model.Fault{
			Kind: model.FaultPacketLoss, Severity: model.SeverityInfo,
			Message: fmt.Sprintf("%d duplicate packets ignored for recovery", stats.Duplicates),
		})
	}
	// Timestamp monotonicity in sequence order: RTP timestamps must not move
	// backwards as sequence numbers advance.
	ordered := make([]*rtp.Packet, len(pkts))
	copy(ordered, pkts)
	sortBySeq(ordered, first)
	for i := 1; i < len(ordered); i++ {
		if seqDiff(ordered[i].SequenceNumber, ordered[i-1].SequenceNumber) <= ReorderWindow &&
			ordered[i].Timestamp < ordered[i-1].Timestamp {
			stats.TimestampGap = true
			stats.Faults = append(stats.Faults, model.Fault{
				Kind: model.FaultRTPScale, Severity: model.SeverityWarning,
				Message: "RTP timestamp moves backwards in sequence order",
			})
			break
		}
	}
	return stats
}

func sortBySeq(pkts []*rtp.Packet, base uint16) {
	// Insertion sort is fine: captures are replayed once and already nearly
	// ordered, and this avoids mutating the caller's slice.
	for i := 1; i < len(pkts); i++ {
		for j := i; j > 0 && seqDiff(pkts[j].SequenceNumber, pkts[j-1].SequenceNumber) < 0; j-- {
			pkts[j], pkts[j-1] = pkts[j-1], pkts[j]
		}
	}
}

// seqDiff returns a-b in 16-bit sequence space, handling wraparound.
func seqDiff(a, b uint16) int {
	return int(int16(a - b))
}
