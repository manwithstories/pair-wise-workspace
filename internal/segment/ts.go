package segment

import (
	"encoding/binary"
	"errors"
	"fmt"

	"avrepair/internal/media"
)

// Transport-stream timestamps run on a 90 kHz clock.
const TSScale int64 = 90000

// Capture container constants. Synthetic fixtures store each MPEG-TS
// packet inside a minimal RTP record so that loss, reordering and
// duplication can be described by sequence numbers.
const (
	RTPHeaderLen     = 12
	TSPacketLen      = 188
	PayloadMPEGTS    = 33
	PayloadParity    = 96
	ParityPayloadLen = 2 + TSPacketLen
	DataRecordLen    = RTPHeaderLen + TSPacketLen
	ParityRecordLen  = RTPHeaderLen + ParityPayloadLen
)

var (
	// ErrBadCapture marks a malformed RTP/TS capture record.
	ErrBadCapture = errors.New("segment: malformed packet capture")
	// ErrBadTS marks a payload that is not a usable transport stream.
	ErrBadTS = errors.New("segment: malformed transport stream")
)

// Packet is one captured RTP record. Data packets carry a 188-byte TS
// packet. Parity packets carry a two-byte group base followed by the
// XOR of the eight data payloads in that group.
type Packet struct {
	Seq     uint16
	Parity  bool
	Base    uint16
	Payload []byte
}

// Sample is a PES timestamp extracted from a TS payload.
type Sample struct {
	Raw      int64
	Scale    int64
	Sync     bool
	StreamID byte
}

// DecodePackets splits a capture file into records. It validates every
// record header, payload size and the fixed RTP version.
func DecodePackets(data []byte) ([]Packet, error) {
	var out []Packet
	for pos := 0; pos < len(data); {
		if len(data)-pos < RTPHeaderLen {
			return nil, fmt.Errorf("%w: short header at %d", ErrBadCapture, pos)
		}
		h := data[pos : pos+RTPHeaderLen]
		if h[0]>>6 != 2 {
			return nil, fmt.Errorf("%w: rtp version %d at %d", ErrBadCapture, h[0]>>6, pos)
		}
		pt := h[1] & 0x7F
		seq := binary.BigEndian.Uint16(h[2:4])
		pos += RTPHeaderLen
		switch pt {
		case PayloadMPEGTS:
			if len(data)-pos < TSPacketLen {
				return nil, fmt.Errorf("%w: short TS record at %d", ErrBadCapture, pos)
			}
			payload := append([]byte(nil), data[pos:pos+TSPacketLen]...)
			if payload[0] != 0x47 {
				return nil, fmt.Errorf("%w: TS sync byte %#x", ErrBadCapture, payload[0])
			}
			out = append(out, Packet{Seq: seq, Payload: payload})
			pos += TSPacketLen
		case PayloadParity:
			if len(data)-pos < ParityPayloadLen {
				return nil, fmt.Errorf("%w: short parity record at %d", ErrBadCapture, pos)
			}
			raw := append([]byte(nil), data[pos:pos+ParityPayloadLen]...)
			out = append(out, Packet{
				Seq:     seq,
				Parity:  true,
				Base:    binary.BigEndian.Uint16(raw[:2]),
				Payload: raw[2:],
			})
			pos += ParityPayloadLen
		default:
			return nil, fmt.Errorf("%w: unknown payload type %d", ErrBadCapture, pt)
		}
	}
	return out, nil
}

// EncodePackets serializes records in the given order.
func EncodePackets(pkts []Packet) []byte {
	size := 0
	for _, p := range pkts {
		if p.Parity {
			size += ParityRecordLen
		} else {
			size += DataRecordLen
		}
	}
	out := make([]byte, 0, size)
	for _, p := range pkts {
		var hdr [RTPHeaderLen]byte
		hdr[0] = 0x80
		pt := byte(PayloadMPEGTS)
		if p.Parity {
			pt = PayloadParity
		}
		hdr[1] = pt
		binary.BigEndian.PutUint16(hdr[2:4], p.Seq)
		out = append(out, hdr[:]...)
		if p.Parity {
			var base [2]byte
			binary.BigEndian.PutUint16(base[:], p.Base)
			out = append(out, base[:]...)
			out = append(out, p.Payload...)
			continue
		}
		out = append(out, p.Payload...)
	}
	return out
}

// IsPlainTS reports whether data starts with a TS sync byte rather than
// an RTP header. Plain TS captures cannot carry sequence-level FEC.
func IsPlainTS(data []byte) bool {
	return len(data) >= TSPacketLen && data[0] == 0x47
}

// SplitPlainTS slices a bare transport stream into 188-byte packets.
func SplitPlainTS(data []byte) ([][]byte, error) {
	if len(data)%TSPacketLen != 0 {
		return nil, fmt.Errorf("%w: size %d not a multiple of 188", ErrBadTS, len(data))
	}
	out := make([][]byte, 0, len(data)/TSPacketLen)
	for pos := 0; pos < len(data); pos += TSPacketLen {
		p := data[pos : pos+TSPacketLen]
		if p[0] != 0x47 {
			return nil, fmt.Errorf("%w: no sync byte at %d", ErrBadTS, pos)
		}
		out = append(out, append([]byte(nil), p...))
	}
	return out, nil
}

// ParseTS extracts PES timestamps from ordered TS packets. Video starts
// are marked sync when the elementary payload carries an H.264 IDR NAL.
func ParseTS(payloads [][]byte) ([]Sample, error) {
	type pesBuf struct {
		pid  uint16
		data []byte
	}
	pending := map[uint16][]byte{}
	var samples []Sample
	flush := func(pid uint16) error {
		buf := pending[pid]
		delete(pending, pid)
		if len(buf) == 0 {
			return nil
		}
		s, ok, err := parsePES(buf)
		if err != nil {
			return err
		}
		if ok {
			samples = append(samples, s)
		}
		return nil
	}
	seen := map[uint16]bool{}
	for i, p := range payloads {
		if len(p) != TSPacketLen || p[0] != 0x47 {
			return nil, fmt.Errorf("%w: packet %d", ErrBadTS, i)
		}
		if p[1]&0x80 != 0 { // transport_error_indicator
			continue
		}
		pid := uint16(p[1]&0x1F)<<8 | uint16(p[2])
		if pid == 0x1FFF {
			continue
		}
		afc := (p[3] >> 4) & 3
		off := 4
		if afc&2 != 0 {
			if off >= len(p) {
				return nil, fmt.Errorf("%w: adaptation field", ErrBadTS)
			}
			off += 1 + int(p[4])
		}
		if afc&1 == 0 || off >= len(p) {
			continue
		}
		pusi := p[1]&0x40 != 0
		if pusi {
			if err := flush(pid); err != nil {
				return nil, err
			}
			pending[pid] = append([]byte(nil), p[off:]...)
			seen[pid] = true
		} else if seen[pid] {
			pending[pid] = append(pending[pid], p[off:]...)
		}
	}
	for pid := range pending {
		if err := flush(pid); err != nil {
			return nil, err
		}
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("%w: no PES timestamps", ErrBadTS)
	}
	return samples, nil
}

func parsePES(buf []byte) (Sample, bool, error) {
	if len(buf) < 9 || buf[0] != 0 || buf[1] != 0 || buf[2] != 1 {
		return Sample{}, false, nil // not a PES start; ignore padding
	}
	streamID := buf[3]
	if streamID == 0xBE || streamID == 0xBF {
		return Sample{}, false, nil
	}
	flags := buf[7]
	dataLen := int(buf[8])
	dataStart := 9 + dataLen
	if dataStart > len(buf) {
		return Sample{}, false, fmt.Errorf("%w: PES header overflow", ErrBadTS)
	}
	if flags&0x80 == 0 {
		return Sample{}, false, nil
	}
	if len(buf) < 14 {
		return Sample{}, false, fmt.Errorf("%w: truncated PTS", ErrBadTS)
	}
	pts := decodePTS(buf[9:14])
	video := streamID >= 0xE0 && streamID <= 0xEF
	audio := streamID >= 0xC0 && streamID <= 0xDF
	if !video && !audio {
		return Sample{}, false, nil
	}
	sync := true
	if video {
		sync = hasIDR(buf[dataStart:])
	}
	return Sample{Raw: pts, Scale: TSScale, Sync: sync, StreamID: streamID}, true, nil
}

func decodePTS(b []byte) int64 {
	return int64(b[0]&0x0E)<<29 |
		int64(b[1])<<22 |
		int64(b[2]&0xFE)<<14 |
		int64(b[3])<<7 |
		int64(b[4]&0xFE)>>1
}

// EncodePTS expands a 33-bit value into the five-byte PES field.
func EncodePTS(pts int64) [5]byte {
	pts &= media.Wrap33 - 1
	return [5]byte{
		byte(0x21 | ((pts >> 29) & 0x0E)),
		byte(pts >> 22),
		byte(0x01 | ((pts >> 14) & 0xFE)),
		byte(pts >> 7),
		byte(0x01 | ((pts << 1) & 0xFE)),
	}
}

func hasIDR(data []byte) bool {
	for i := 0; i+4 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			nal := data[i+3] & 0x1F
			if nal == 5 {
				return true
			}
			i += 2
		}
	}
	return false
}
