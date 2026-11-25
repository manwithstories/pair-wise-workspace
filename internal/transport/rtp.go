package transport

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// DatagramKind distinguishes media datagrams from XOR parity datagrams.
type DatagramKind uint8

const (
	KindMedia DatagramKind = iota
	KindParity
)

// Datagram is one received UDP datagram on the simulated link, already
// tagged with the track it belongs to and its arrival time on the shared
// virtual clock.
type Datagram struct {
	Track     uint64
	Bytes     []byte
	ArrivalNs int64
	Kind      DatagramKind
}

// Payload type reserved for parity datagrams.
const ParityPayloadType = 127

// parityMagic prefixes the XOR parity header. The layout after the magic is:
// base sequence (4), base timestamp (4), group size (1), missing count (1),
// per-slot tick step (4), then the XOR bytes.
var parityMagic = [4]byte{'A', 'V', 'P', '1'}

// parityHeaderLen is the fixed portion of a parity payload.
const parityHeaderLen = 18

// DecodeMedia parses an RTP datagram with pion/rtp and maps it onto the
// buffer's Packet model. The RTP timestamp carries source ticks; the caller
// converts ticks to nanoseconds with the document's timecode scale.
func DecodeMedia(d Datagram) (Packet, error) {
	var p rtp.Packet
	if err := p.Unmarshal(d.Bytes); err != nil {
		return Packet{}, fmt.Errorf("transport: rtp decode for track %d: %w", d.Track, err)
	}
	if len(p.Payload) == 0 {
		return Packet{}, errors.New("transport: empty rtp payload")
	}
	return Packet{
		Stream:    uint16(d.Track),
		Seq:       uint32(p.SequenceNumber),
		Timestamp: p.Timestamp,
		Marker:    p.Marker,
		Payload:   p.Payload,
		ArrivalNs: d.ArrivalNs,
	}, nil
}

// ParityHeader is the metadata carried by a parity datagram: which group (by
// base sequence and base timestamp) and how many slots it protects.
type ParityHeader struct {
	BaseSeq   uint32
	BaseTs    uint32
	GroupSize uint8
	Missing   uint8
	// StepTicks is the per-slot timestamp increment used to place a recovered
	// packet on the timeline without inventing a time from nothing.
	StepTicks uint32
}

// DecodeParity parses a parity datagram into its header and XOR bytes.
func DecodeParity(d Datagram) (ParityHeader, []byte, error) {
	var h ParityHeader
	var p rtp.Packet
	if err := p.Unmarshal(d.Bytes); err != nil {
		return h, nil, fmt.Errorf("transport: parity rtp decode for track %d: %w", d.Track, err)
	}
	if p.PayloadType != ParityPayloadType {
		return h, nil, fmt.Errorf("transport: datagram has payload type %d, not parity", p.PayloadType)
	}
	if len(p.Payload) < parityHeaderLen || [4]byte(p.Payload[:4]) != parityMagic {
		return h, nil, errors.New("transport: malformed parity header")
	}
	b := p.Payload[4:]
	h.BaseSeq = binary.BigEndian.Uint32(b[0:4])
	h.BaseTs = binary.BigEndian.Uint32(b[4:8])
	h.GroupSize = b[8]
	h.Missing = b[9]
	h.StepTicks = binary.BigEndian.Uint32(b[10:14])
	return h, p.Payload[parityHeaderLen:], nil
}

// EncodeMedia builds an RTP datagram for a payload, used by fixtures and
// tests. Timestamp carries source ticks and sequence numbers advance by one.
func EncodeMedia(track uint64, seq, ts uint32, marker bool, payload []byte) ([]byte, error) {
	p := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: uint16(seq),
			Timestamp:      ts,
			SSRC:           uint32(track),
			Marker:         marker,
		},
		Payload: payload,
	}
	return p.Marshal()
}

// EncodeParity builds the parity datagram for a group.
func EncodeParity(track uint64, seq, ts uint32, h ParityHeader, xor []byte) ([]byte, error) {
	body := make([]byte, parityHeaderLen+len(xor))
	copy(body, parityMagic[:])
	binary.BigEndian.PutUint32(body[4:], h.BaseSeq)
	binary.BigEndian.PutUint32(body[8:], h.BaseTs)
	body[12] = h.GroupSize
	body[13] = h.Missing
	binary.BigEndian.PutUint32(body[14:], h.StepTicks)
	copy(body[parityHeaderLen:], xor)
	p := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    ParityPayloadType,
			SequenceNumber: uint16(seq),
			Timestamp:      ts,
			SSRC:           uint32(track),
		},
		Payload: body,
	}
	return p.Marshal()
}

// XOR folds every payload into dst (dst must hold the longest payload length;
// shorter payloads are zero-padded, matching the repairer's assumption).
func XOR(dst []byte, payloads ...[]byte) {
	for _, p := range payloads {
		for i := 0; i < len(dst) && i < len(p); i++ {
			dst[i] ^= p[i]
		}
	}
}
