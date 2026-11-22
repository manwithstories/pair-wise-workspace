// Package rtp validates, serializes and parses RTP packets on top of
// github.com/pion/rtp and maps them onto the unified media clock as
// model.Event values. It also owns per-stream duplicate/conflict detection.
package rtp

import (
	"errors"
	"hash/fnv"

	"github.com/pion/rtp"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// MaxPayloadBytes bounds memory allocated for a single packet. Length fields
// claiming more are rejected before any allocation of that size.
const MaxPayloadBytes = 8192

// MinHeaderSize is the smallest legal fixed RTP header.
const MinHeaderSize = 12

// ParseOptions controls clock mapping of incoming packets.
type ParseOptions struct {
	// ClockRate is the RTP timestamp rate for the stream (e.g. 48k/90k).
	ClockRate uint32
	// Kind is attached to every emitted event.
	Kind model.Kind
}

// Parse unmarshals one wire packet, validates its structure, and returns an
// event anchored on the unified timeline. RTP timestamps wrap at 32 bits; the
// caller's stream state (see Tracker) is responsible for wrap extension.
func Parse(wire []byte, arrival clock.Ticks, opts ParseOptions) (*model.Event, error) {
	if len(wire) < MinHeaderSize {
		return nil, &model.MediaError{
			Category: model.CatRTPHeader,
			Detail:   "packet shorter than 12-byte RTP header",
			Decision: model.DecRejected,
		}
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(wire); err != nil {
		return nil, &model.MediaError{
			Category: model.CatRTPHeader,
			Detail:   "rtp unmarshal failed: " + err.Error(),
			Decision: model.DecRejected,
		}
	}
	if len(pkt.Payload) > MaxPayloadBytes {
		return nil, &model.MediaError{
			Category: model.CatRTPHeader,
			Stream:   pkt.SSRC,
			Seq:      pkt.SequenceNumber,
			Detail:   "payload length exceeds safety bound",
			Decision: model.DecRejected,
		}
	}
	if opts.ClockRate == 0 {
		return nil, &model.MediaError{
			Category: model.CatRTPHeader,
			Stream:   pkt.SSRC,
			Seq:      pkt.SequenceNumber,
			Detail:   "zero clock rate cannot map timestamps",
			Decision: model.DecRejected,
		}
	}
	h := PayloadHash(pkt.Payload)
	ev := &model.Event{
		Kind:        opts.Kind,
		Stream:      pkt.SSRC,
		Seq:         pkt.SequenceNumber,
		EventTime:   clock.FromRTP(uint64(pkt.Timestamp), opts.ClockRate),
		Arrival:     arrival,
		PayloadHash: h,
	}
	if opts.Kind == model.KindAudio {
		ev.AudioPCM = append([]byte(nil), pkt.Payload...)
	}
	return ev, nil
}

// PayloadHash returns a stable 64-bit FNV-1a hash used to distinguish
// identical retransmissions from same-sequence payload conflicts.
func PayloadHash(payload []byte) uint64 {
	h := fnv.New64a()
	h.Write(payload)
	return h.Sum64()
}

// Marshal wraps a payload into a serialized RTP packet with the given header
// fields. It is the deterministic inverse of Parse for fixture generation.
func Marshal(ssrc uint32, seq uint16, rtpTS uint32, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, errors.New("rtp: payload exceeds safety bound")
	}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      rtpTS,
			SSRC:           ssrc,
		},
		Payload: append([]byte(nil), payload...),
	}
	return pkt.Marshal()
}
