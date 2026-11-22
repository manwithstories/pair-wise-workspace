package stream

import (
	"github.com/pion/rtp"

	"mediascan/internal/media"
)

// RTPLimits bound RTP packet analysis.
type RTPLimits struct {
	MaxPackets int
	// MaxPayload caps a single packet's payload length.
	MaxPayload int
}

// DefaultRTPLimits bound generated fixtures and accidental large inputs.
var DefaultRTPLimits = RTPLimits{MaxPackets: 100_000, MaxPayload: 64 << 10}

func (l RTPLimits) normalize() RTPLimits {
	if l.MaxPackets <= 0 {
		l.MaxPackets = DefaultRTPLimits.MaxPackets
	}
	if l.MaxPayload <= 0 {
		l.MaxPayload = DefaultRTPLimits.MaxPayload
	}
	return l
}

// RTPPacket is the analysis result for one packet in arrival order.
type RTPPacket struct {
	// Index is the zero-based arrival index.
	Index int
	// Seq is the header sequence number.
	Seq uint16
	// Timestamp is the raw 32-bit RTP timestamp (media clock ticks).
	Timestamp uint32
	// TimestampNs is the timestamp scaled to the shared base; only valid when
	// ClockRate is set on the analysis.
	TimestampNs int64
	// Marker, PayloadType, SSRC are header fields kept for diagnostics.
	Marker      bool
	PayloadType uint8
	SSRC        uint32
	PayloadLen  int
}

// RTPAnalysis is the result of inspecting an RTP packet sequence.
type RTPAnalysis struct {
	// ClockRate is the media clock rate used to scale timestamps to ns; zero
	// means timestamps were not scaled.
	ClockRate uint32
	Packets   []RTPPacket
	Issues    media.IssueSet
	// Recoverable describes the contiguous arrival ranges that remain
	// trustworthy after applying the error model.
	Recovered []media.Range
	// Wraps/Duplicates/OutOfOrder counts are stable counters for replay
	// comparison.
	Wraps      int
	Duplicates int
	OutOfOrder int
	Truncated  int
}

// AnalyzeRTP parses packets serialized in arrival order and reports sequence
// wrap-around, duplicates, out-of-order delivery and truncated packets through
// the unified error model. Truncated packets are isolated to their arrival
// index; the surrounding sequence continues to be analyzed.
func AnalyzeRTP(packets [][]byte, clockRate uint32, limits RTPLimits) *RTPAnalysis {
	limits = limits.normalize()
	a := &RTPAnalysis{ClockRate: clockRate}
	if int64(len(packets)) > int64(limits.MaxPackets) {
		a.Issues.Add(media.NewFatal(media.ClassRTPInvalidHeader,
			media.Locator{PacketIndex: limits.MaxPackets},
			"packet count %d exceeds limit %d", len(packets), limits.MaxPackets))
		return a
	}
	seen := make(map[uint16]int, len(packets))
	var prevSeq uint16
	havePrev := false
	for i, raw := range packets {
		var pkt rtp.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			a.Truncated++
			a.Issues.Add(media.NewIssue(media.ClassRTPTruncated, media.Locator{PacketIndex: i},
				"packet %d cannot be parsed: %v", i, err))
			continue
		}
		if pkt.Version != 2 {
			a.Issues.Add(media.NewIssue(media.ClassRTPInvalidHeader, media.Locator{PacketIndex: i, PacketSeq: pkt.SequenceNumber},
				"packet %d has RTP version %d, want 2", i, pkt.Version))
		}
		if len(pkt.Payload) > limits.MaxPayload {
			a.Issues.Add(media.NewIssue(media.ClassRTPInvalidHeader, media.Locator{PacketIndex: i, PacketSeq: pkt.SequenceNumber},
				"packet %d payload %d exceeds limit %d", i, len(pkt.Payload), limits.MaxPayload))
			continue
		}
		seq := pkt.SequenceNumber
		if havePrev {
			// Sequence comparison with wrap handling. diff is the signed
			// 16-bit distance from prevSeq to seq; the natural wrap from
			// 65535 to 0 yields diff == 1.
			diff := int16(seq - prevSeq)
			switch {
			case diff == 0:
				a.Duplicates++
				a.Issues.Add(media.NewInfo(media.ClassRTPDuplicate, media.Locator{PacketIndex: i, PacketSeq: seq},
					"duplicate of sequence %d", seq))
			case diff == 1 && seq < prevSeq:
				a.Wraps++
				a.Issues.Add(media.NewInfo(media.ClassRTPSequenceWrap, media.Locator{PacketIndex: i, PacketSeq: seq},
					"sequence wrapped from %d to %d", prevSeq, seq))
			case diff < 0:
				a.OutOfOrder++
				a.Issues.Add(media.NewInfo(media.ClassRTPOutOfOrder, media.Locator{PacketIndex: i, PacketSeq: seq},
					"sequence %d arrived after %d (out of order)", seq, prevSeq))
			}
		}
		// Track first arrival per sequence number. The havePrev diff already
		// counted an adjacent duplicate, so only report a repeat seen earlier
		// (not the immediately preceding packet) to avoid double counting.
		if first, ok := seen[seq]; ok {
			if i-1 != first {
				a.Duplicates++
				a.Issues.Add(media.NewInfo(media.ClassRTPDuplicate, media.Locator{PacketIndex: i, PacketSeq: seq},
					"sequence %d already seen at packet %d", seq, first))
			}
		} else {
			seen[seq] = i
		}
		rp := RTPPacket{
			Index: i, Seq: seq, Timestamp: pkt.Timestamp,
			Marker: pkt.Marker, PayloadType: pkt.PayloadType, SSRC: pkt.SSRC,
			PayloadLen: len(pkt.Payload),
		}
		if clockRate > 0 {
			ns, err := media.ScaleToNanos(int64(pkt.Timestamp), clockRate)
			if err != nil {
				a.Issues.Add(media.NewIssue(media.ClassRTPInvalidHeader, media.Locator{PacketIndex: i, PacketSeq: seq},
					"timestamp %d overflows at clock rate %d", pkt.Timestamp, clockRate))
			} else {
				rp.TimestampNs = ns
			}
		}
		a.Packets = append(a.Packets, rp)
		if !havePrev || int16(seq-prevSeq) >= 0 {
			prevSeq = seq
			havePrev = true
		}
	}
	a.Recovered = recoverableRanges(a)
	return a
}

// recoverableRanges describes which arrival spans remain usable. Truncated
// packets split the arrival sequence; each maximal run of parseable packets
// becomes a recoverable range, so callers know exactly which packet indices
// survived.
func recoverableRanges(a *RTPAnalysis) []media.Range {
	bad := map[int]bool{}
	total := 0
	for _, is := range a.Issues {
		if is.Class != media.ClassRTPTruncated {
			continue
		}
		bad[is.Locator.PacketIndex] = true
		if is.Locator.PacketIndex+1 > total {
			total = is.Locator.PacketIndex + 1
		}
	}
	for _, p := range a.Packets {
		if p.Index+1 > total {
			total = p.Index + 1
		}
	}
	var out []media.Range
	start := -1
	for i := 0; i < total; i++ {
		if bad[i] {
			if start >= 0 {
				out = append(out, media.Range{
					TrackKind: "rtp", Class: media.ClassRTPTruncated,
					PacketStart: start, PacketEnd: i, Recoverable: true,
					Detail: "intact packet run",
				})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, media.Range{
			TrackKind: "rtp", Class: media.ClassRTPTruncated,
			PacketStart: start, PacketEnd: total, Recoverable: true,
			Detail: "intact packet run",
		})
	}
	return out
}
