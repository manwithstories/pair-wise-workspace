// Package stream parses self-generated RTP/RTCP datagrams and tracks per-track
// sequence state: 16-bit wrap, loss, duplicates, payload conflicts and
// reordering. Unparseable RTCP is reported as an error and never touches the
// media timeline.
package stream

import (
	"hash/fnv"

	"github.com/pion/rtp"

	"mediapipeline/internal/mediaerr"
)

// RTCP dynamic payload-type range used for RTP/RTCP multiplexing (RFC 5761).
const (
	rtcpPTMin       = 64
	rtcpPTMax       = 95
	rtpHeaderMinLen = 12
)

// Packet is the validated view of one RTP datagram.
type Packet struct {
	TrackID     uint32 // SSRC, used as the track identity
	Raw         []byte // original datagram bytes
	Version     uint8
	Padding     bool
	Marker      bool
	PayloadType uint8
	Sequence    uint16
	Timestamp   uint32
	SSRC        uint32
	CSRC        []uint32
	Payload     []byte
	// ExtSeq is the 64-bit expansion of Sequence, filled by Tracker.
	ExtSeq uint64
	// Offset is the byte offset of the datagram inside the source stream.
	Offset int64
	// Arrival is a monotonic per-stream arrival index.
	Arrival int64
}

// IsRTCP reports whether a datagram should be demultiplexed as RTCP.
func IsRTCP(buf []byte) bool {
	if len(buf) < 2 {
		return false
	}
	pt := buf[1] & 0x7F
	return pt >= rtcpPTMin && pt <= rtcpPTMax
}

// ParseRTCP validates a (possibly compound) RTCP datagram by walking every
// block header. It returns a structured error on malformed input and never
// mutates any media timeline state.
func ParseRTCP(buf []byte) *mediaerr.Error {
	if len(buf) < 4 {
		return mediaerr.New(mediaerr.StageParse, mediaerr.KindInvalidRTCP,
			"RTCP datagram shorter than 4 bytes").WithOffset(int64(len(buf)))
	}
	if buf[0]>>6 != 2 {
		return mediaerr.New(mediaerr.StageParse, mediaerr.KindInvalidRTCP,
			"RTCP version is not 2")
	}
	padding := buf[0]&0x20 != 0
	for off := 0; off < len(buf); {
		if len(buf)-off < 4 {
			return mediaerr.New(mediaerr.StageParse, mediaerr.KindInvalidRTCP,
				"truncated RTCP block header").WithOffset(int64(off))
		}
		words := int(uint16(buf[off+2])<<8 | uint16(buf[off+3]))
		blockLen := (words + 1) * 4
		if blockLen == 0 || off+blockLen > len(buf) {
			return mediaerr.New(mediaerr.StageParse, mediaerr.KindInvalidRTCP,
				"RTCP length field exceeds datagram").WithOffset(int64(off))
		}
		if padding && off+blockLen == len(buf) {
			pad := int(buf[len(buf)-1])
			if pad == 0 || pad > blockLen {
				return mediaerr.New(mediaerr.StageParse, mediaerr.KindInvalidRTCP,
					"RTCP padding length invalid").WithOffset(int64(off))
			}
		}
		off += blockLen
	}
	return nil
}

// ParseRTP validates and decodes one RTP datagram. Truncated input is located
// with the source offset; header, extension and payload lengths are all
// checked before the payload is exposed.
func ParseRTP(buf []byte, offset, arrival int64) (*Packet, *mediaerr.Error) {
	if len(buf) < rtpHeaderMinLen {
		return nil, mediaerr.New(mediaerr.StageParse, mediaerr.KindTruncated,
			"datagram shorter than RTP fixed header").WithOffset(offset)
	}
	if buf[0]>>6 != 2 {
		return nil, mediaerr.New(mediaerr.StageParse, mediaerr.KindInvalidHeader,
			"RTP version is not 2").WithOffset(offset)
	}
	csrcCount := int(buf[0] & 0x0F)
	hasExt := buf[0]&0x10 != 0
	padding := buf[0]&0x20 != 0
	headerLen := rtpHeaderMinLen + 4*csrcCount
	if hasExt {
		headerLen += 4 // extension header itself
	}
	if len(buf) < headerLen {
		return nil, mediaerr.New(mediaerr.StageParse, mediaerr.KindTruncated,
			"RTP header truncated before declared CSRC/extension").
			WithOffset(offset)
	}
	var pionPkt rtp.Packet
	if err := pionPkt.Unmarshal(buf); err != nil {
		kind := mediaerr.KindInvalidHeader
		if len(buf) < headerLen+1 {
			kind = mediaerr.KindTruncated
		}
		return nil, mediaerr.Wrap(mediaerr.StageParse, kind,
			"RTP unmarshal failed", err).WithOffset(offset).
			WithTrack(safeSSRC(buf))
	}
	if hasExt {
		// pion already validated the extension length; double-check payload fit.
		if len(pionPkt.Payload) == 0 && !padding {
			return nil, mediaerr.New(mediaerr.StageParse, mediaerr.KindPayloadLength,
				"extension leaves no payload and no padding").
				WithOffset(offset).WithTrack(pionPkt.SSRC).WithSeq(pionPkt.SequenceNumber)
		}
	}
	// pion's Unmarshal already validates the padding byte against the
	// datagram length (errTooSmall) and strips it from Payload, so here we
	// only retain the flag; re-stripping would corrupt valid packets.
	return &Packet{
		TrackID:     pionPkt.SSRC,
		Raw:         append([]byte(nil), buf...),
		Version:     2,
		Padding:     padding,
		Marker:      pionPkt.Marker,
		PayloadType: pionPkt.PayloadType,
		Sequence:    pionPkt.SequenceNumber,
		Timestamp:   pionPkt.Timestamp,
		SSRC:        pionPkt.SSRC,
		CSRC:        append([]uint32(nil), pionPkt.CSRC...),
		Payload:     append([]byte(nil), pionPkt.Payload...),
		Offset:      offset,
		Arrival:     arrival,
	}, nil
}

func safeSSRC(buf []byte) uint32 {
	if len(buf) < 12 {
		return 0
	}
	return uint32(buf[8])<<24 | uint32(buf[9])<<16 | uint32(buf[10])<<8 | uint32(buf[11])
}

// payloadHash gives a compact content identity for duplicate-conflict checks.
func payloadHash(b []byte) uint32 {
	h := fnv.New32a()
	h.Write(b)
	return h.Sum32()
}

// SeqEvent classifies the outcome of observing one sequence number.
type SeqEvent int

// Sequence observation outcomes.
const (
	SeqFirst SeqEvent = iota
	SeqInOrder
	SeqGap
	SeqReordered
	SeqDuplicate
)

// Stats accumulates per-track sequence counters.
type Stats struct {
	Received   int64
	Duplicates int64
	Conflicts  int64
	Reordered  int64
	GapEvents  int64
	Lost       int64
	FirstExt   uint64
	MaxExt     uint64
}

// Tracker expands 16-bit sequence numbers to 64 bits per track and records
// loss, duplicate, conflict and reorder statistics.
type Tracker struct {
	track   uint32
	started bool
	first   uint64
	maxExt  uint64
	seen    map[uint64]uint32 // ext seq -> payload hash
	stats   Stats
}

// NewTracker creates a tracker for one SSRC/track.
func NewTracker(track uint32) *Tracker {
	return &Tracker{track: track, seen: make(map[uint64]uint32)}
}

// Observe expands the packet's 16-bit sequence number, records statistics and
// writes the extended value back into the packet. A duplicate sequence with a
// differing payload is counted as a conflict; such packets must be isolated
// downstream rather than silently overwriting the original.
func (t *Tracker) Observe(p *Packet) (SeqEvent, *mediaerr.Error) {
	if p.SSRC != t.track {
		return 0, mediaerr.New(mediaerr.StageParse, mediaerr.KindInvalidInput,
			"packet SSRC does not match tracker track").
			WithTrack(t.track).WithSeq(p.Sequence)
	}
	var ext uint64
	ev := SeqFirst
	if !t.started {
		t.started = true
		t.first = uint64(p.Sequence)
		t.maxExt = t.first
		ext = t.first
		t.stats.FirstExt = ext
	} else {
		ext = extendSeq(t.maxExt, p.Sequence)
		switch {
		case ext == t.maxExt && t.seenContains(ext):
			ev = SeqDuplicate
		case ext < t.maxExt:
			ev = SeqReordered
		case ext > t.maxExt:
			if ext-t.maxExt > 1 {
				ev = SeqGap
				t.stats.GapEvents += int64(ext - t.maxExt - 1)
			} else {
				ev = SeqInOrder
			}
			t.maxExt = ext
		}
	}
	hash := payloadHash(p.Payload)
	if prior, ok := t.seen[ext]; ok {
		t.stats.Duplicates++
		if prior != hash {
			t.stats.Conflicts++
			return ev, mediaerr.New(mediaerr.StageParse, mediaerr.KindDuplicate,
				"duplicate sequence number with conflicting payload").
				WithTrack(t.track).WithSeq(p.Sequence).WithTS(p.Timestamp)
		}
		if ev != SeqDuplicate {
			ev = SeqDuplicate
		}
	} else {
		t.seen[ext] = hash
		t.stats.Received++
	}
	if ev == SeqReordered {
		t.stats.Reordered++
	}
	p.ExtSeq = ext
	t.stats.MaxExt = t.maxExt
	return ev, nil
}

func (t *Tracker) seenContains(ext uint64) bool { _, ok := t.seen[ext]; return ok }

// Flush finalizes loss statistics over the whole observed span and returns a
// snapshot. Missing sequence numbers between the first and most-recent packet
// that never arrived are counted as lost.
func (t *Tracker) Flush() Stats {
	s := t.stats
	if t.started {
		span := int64(t.maxExt - t.first + 1)
		if span > s.Received {
			s.Lost = span - s.Received
		}
	}
	return s
}

// Stats returns the current (non-finalized) counters.
func (t *Tracker) Stats() Stats { return t.stats }

// extendSeq expands a 16-bit sequence relative to the largest extended value.
func extendSeq(ref uint64, seq uint16) uint64 {
	const mod int64 = 1 << 16
	const half = mod / 2
	delta := int64(seq) - int64(ref%uint64(mod))
	if delta > half {
		delta -= mod
	} else if delta < -half {
		delta += mod
	}
	return uint64(int64(ref) + delta)
}
