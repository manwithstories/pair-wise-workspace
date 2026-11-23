// Package rtp receives RTP packet streams, isolates them by SSRC, extends
// wrap-around sequence numbers and timestamps, and drops duplicates
// idempotently.
package rtp

import (
	"encoding/binary"
	"fmt"

	pionrtp "github.com/pion/rtp"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
)

// dedupWindow bounds how many extended sequence numbers are remembered
// for idempotent duplicate suppression.
const dedupWindow = 4096

// Stats counts receiver-level observations.
type Stats struct {
	PacketsIn  int
	Duplicates int
	Malformed  int
	Foreign    int // datagrams belonging to another SSRC
}

// Receiver processes the RTP datagram stream of one SSRC.
type Receiver struct {
	ssrc   uint32
	init   bool
	maxSeq uint64
	maxTS  uint64
	seen   map[uint64]struct{}
	order  []uint64
	Stats  Stats
}

// NewReceiver creates a receiver isolated to the given SSRC.
func NewReceiver(ssrc uint32) *Receiver {
	return &Receiver{ssrc: ssrc, seen: make(map[uint64]struct{})}
}

// Accept parses one raw RTP datagram. Exactly one of the return values is
// meaningful:
//   - a packet event ready for the timeline,
//   - a quarantine record (header parsed, payload unusable),
//   - an error (undecodable codec config or malformed datagram).
//
// Duplicates and foreign-SSRC datagrams are counted and return all nil.
func (r *Receiver) Accept(raw []byte, arrival clock.Time, cfg event.CodecConfig) (*event.Packet, *event.Quarantine, error) {
	r.Stats.PacketsIn++
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	var pkt pionrtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		r.Stats.Malformed++
		return nil, nil, &event.Error{Op: "rtp.unmarshal", Reason: event.ReasonRTPMalformed, Err: err}
	}
	if pkt.SSRC != r.ssrc {
		r.Stats.Foreign++
		return nil, nil, nil
	}
	if !r.init {
		r.maxSeq = uint64(pkt.SequenceNumber)
		r.maxTS = uint64(pkt.Timestamp)
		r.init = true
	}
	seq := clock.Unwrap16(r.maxSeq, pkt.SequenceNumber)
	ts := clock.Unwrap32(r.maxTS, pkt.Timestamp)
	if seq > r.maxSeq {
		r.maxSeq = seq
	}
	if ts > r.maxTS {
		r.maxTS = ts
	}
	if _, dup := r.seen[seq]; dup {
		r.Stats.Duplicates++
		return nil, nil, nil
	}
	r.remember(seq)
	dur, err := PacketDuration(cfg, pkt.Payload)
	if err != nil {
		reason := event.ReasonTruncated
		if ee, ok := err.(*event.Error); ok {
			reason = ee.Reason
		}
		return nil, &event.Quarantine{
			SSRC: pkt.SSRC, SeqExt: seq, TSExt: ts, HasSeq: true,
			Reason: reason, Detail: err.Error(),
		}, nil
	}
	p := &event.Packet{
		SSRC:      pkt.SSRC,
		SeqExt:    seq,
		TSExt:     ts,
		MediaTime: clock.FromTicks(ts, cfg.ClockRate),
		Duration:  clock.TicksToDuration(dur, cfg.ClockRate),
		Payload:   pkt.Payload,
		Arrival:   arrival,
	}
	if seq < r.maxSeq {
		p.Flags |= event.FlagReordered
	}
	return p, nil, nil
}

func (r *Receiver) remember(seq uint64) {
	r.seen[seq] = struct{}{}
	r.order = append(r.order, seq)
	if len(r.order) > dedupWindow {
		delete(r.seen, r.order[0])
		r.order = r.order[1:]
	}
}

// PacketDuration derives a packet's duration in codec-clock ticks from the
// codec configuration and the payload's own headers. It never assumes a
// fixed sample count: Opus durations come from the TOC byte (RFC 6716),
// AAC durations from the AU-header table (RFC 3640).
func PacketDuration(cfg event.CodecConfig, payload []byte) (uint64, error) {
	switch cfg.Codec {
	case event.CodecOpus:
		return opusDuration(payload)
	case event.CodecAAC:
		return aacDuration(payload)
	}
	return 0, &event.Error{Op: "rtp.duration", Reason: event.ReasonUndecodableConfig,
		Err: fmt.Errorf("codec %s", cfg.Codec)}
}

// opusFrameTicks returns the Opus frame duration in 48 kHz ticks for a TOC
// config number (RFC 6716 section 3.1).
func opusFrameTicks(cfgNum uint8) uint64 {
	switch {
	case cfgNum < 12: // SILK NB/MB/WB: 10, 20, 40, 60 ms
		return [4]uint64{480, 960, 1920, 2880}[cfgNum%4]
	case cfgNum < 16: // Hybrid SWB/FB: 10, 20 ms
		return [2]uint64{480, 960}[(cfgNum-12)%2]
	default: // CELT NB/WB/SWB/FB: 2.5, 5, 10, 20 ms
		return [4]uint64{120, 240, 480, 960}[cfgNum%4]
	}
}

func opusDuration(p []byte) (uint64, error) {
	if len(p) < 1 {
		return 0, &event.Error{Op: "rtp.opus", Reason: event.ReasonTruncated,
			Err: fmt.Errorf("missing TOC byte")}
	}
	toc := p[0]
	frame := opusFrameTicks(toc >> 3)
	var frames uint64
	switch toc & 0x3 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 {
			return 0, &event.Error{Op: "rtp.opus", Reason: event.ReasonTruncated,
				Err: fmt.Errorf("missing frame-count byte")}
		}
		frames = uint64(p[1] & 0x3F)
		if frames == 0 {
			return 0, &event.Error{Op: "rtp.opus", Reason: event.ReasonUndecodableConfig,
				Err: fmt.Errorf("zero Opus frame count")}
		}
	}
	return frame * frames, nil
}

// auHeaderBits is the AU-header size of the stream configuration this
// archiver handles: 13 bits AU-size plus 3 bits AU-Index (RFC 3640).
const auHeaderBits = 16

// aacSamplesPerFrame is the AAC-LC frame length in samples.
const aacSamplesPerFrame = 1024

func aacDuration(p []byte) (uint64, error) {
	if len(p) < 2 {
		return 0, &event.Error{Op: "rtp.aac", Reason: event.ReasonTruncated,
			Err: fmt.Errorf("missing AU-headers-length field")}
	}
	auBits := binary.BigEndian.Uint16(p)
	if auBits == 0 {
		return aacSamplesPerFrame, nil // single AU, no header table
	}
	if auBits%auHeaderBits != 0 {
		return 0, &event.Error{Op: "rtp.aac", Reason: event.ReasonUndecodableConfig,
			Err: fmt.Errorf("AU-headers-length %d not a multiple of %d", auBits, auHeaderBits)}
	}
	n := uint64(auBits / auHeaderBits)
	if 2+n*2 > uint64(len(p)) {
		return 0, &event.Error{Op: "rtp.aac", Reason: event.ReasonTruncated,
			Err: fmt.Errorf("AU-header table of %d entries exceeds payload", n)}
	}
	return n * aacSamplesPerFrame, nil
}
