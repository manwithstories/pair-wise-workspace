// Package rtp parses Pion RTP packets, reorders/deduplicates them across
// sequence wrap-around and repairs bounded packet loss using retransmission
// records and group (XOR) parity. Unrecoverable holes become explicit gaps
// rather than silent splices.
package rtp

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/pion/rtp"

	"archive/internal/model"
)

// MediaPacket is the internal representation of one RTP packet used by the
// archive pipeline. It keeps the Pion header/payload plus the unwrapped
// sequence number, shared-clock timestamp and on-wire size.
type MediaPacket struct {
	Raw      *rtp.Packet
	SSRC     uint32
	Seq      uint16 // raw 16-bit RTP sequence number
	ExtSeq   int64  // unwrapped monotonic sequence
	TS       uint32 // raw RTP timestamp
	NanoTS   model.Nano
	Payload  []byte
	Size     int
	Retrans  bool // served from the retransmission cache
	Recovered bool // rebuilt by group parity
}

// TrailerLen is the trailing CRC-32 checksum the fixture appends to every
// media payload. The checksum covers the bytes preceding the trailer.
const TrailerLen = 4

// Parser turns raw RTP datagrams into MediaPackets under one shared clock.
// Oversize datagrams are rejected against model.MaxPacketBytes so the
// bounded buffer guarantees cannot be bypassed by a malicious sender.
type Parser struct {
	clock  *model.Clock
	seq    *model.SeqExt
	log    *model.FaultLog
	maxLen int
}

// NewParser builds a parser for the given RTP clock rate.
func NewParser(clockRate uint32, log *model.FaultLog) *Parser {
	return &Parser{
		clock:  model.NewClock(clockRate),
		seq:    model.NewSeqExt(),
		log:    log,
		maxLen: model.MaxPacketBytes,
	}
}

// Parse unmarshals one datagram. The returned packet carries the unwrapped
// sequence and normalised nanosecond timestamp. A CRC mismatch is recorded
// as a locatable fault but does not drop the packet: downstream recovery
// decides whether a retransmission can replace it.
func (p *Parser) Parse(buf []byte) (*MediaPacket, error) {
	if len(buf) == 0 {
		return nil, model.InvariantError(model.PhaseRTP, "empty datagram")
	}
	if len(buf) > p.maxLen {
		p.log.Add(model.Fault{Phase: model.PhaseRTP, Kind: model.FaultOversize,
			Detail: fmt.Sprintf("datagram %d bytes exceeds %d", len(buf), p.maxLen)})
		return nil, model.InvariantError(model.PhaseRTP, "datagram exceeds %d bytes", p.maxLen)
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(buf); err != nil {
		return nil, model.InvariantError(model.PhaseRTP, "unmarshal failed: %v", err)
	}
	if len(pkt.Payload) > model.MaxPayloadBytes {
		p.log.Add(model.Fault{Phase: model.PhaseRTP, Kind: model.FaultOversize,
			Detail: fmt.Sprintf("payload %d bytes exceeds %d", len(pkt.Payload), model.MaxPayloadBytes)})
		return nil, model.InvariantError(model.PhaseRTP, "payload exceeds %d bytes", model.MaxPayloadBytes)
	}
	mp := &MediaPacket{
		Raw:     &pkt,
		SSRC:    pkt.SSRC,
		Seq:     pkt.SequenceNumber,
		ExtSeq:  p.seq.Extend(pkt.SequenceNumber),
		TS:      pkt.Timestamp,
		NanoTS:  p.clock.ToNano(pkt.Timestamp),
		Payload: pkt.Payload,
		Size:    len(buf),
	}
	return mp, nil
}

// CheckCRC validates the trailing CRC-32 (IEEE) of a media payload. Payloads
// shorter than the trailer are treated as failures. Recovered packets are
// re-checked by the same routine so corruption cannot enter via the FEC
// path unnoticed.
func CheckCRC(payload []byte) bool {
	if len(payload) <= TrailerLen {
		return false
	}
	want := binary.BigEndian.Uint32(payload[len(payload)-TrailerLen:])
	return crc32.ChecksumIEEE(payload[:len(payload)-TrailerLen]) == want
}

// MakeCRCTrailer returns the 4-byte big-endian CRC-32 covering data.
func MakeCRCTrailer(data []byte) []byte {
	b := make([]byte, TrailerLen)
	binary.BigEndian.PutUint32(b, crc32.ChecksumIEEE(data))
	return b
}
