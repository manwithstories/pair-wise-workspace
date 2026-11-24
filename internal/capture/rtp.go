package capture

import (
	"github.com/pion/rtp"

	"mediarig/internal/media"
)

// PayloadFormat is the explicit set of RTP payload formats accepted by the
// rig. Fixtures generate L16 PCM and constrained H.264 only.
type PayloadFormat string

const (
	PayloadL16     PayloadFormat = "L16"
	PayloadH264    PayloadFormat = "H264"
	PayloadUnknown PayloadFormat = "unknown"
)

// PayloadSpec binds a dynamic payload type to a media format and RTP clock.
type PayloadSpec struct {
	PayloadType uint8
	Format      PayloadFormat
	ClockRate   int
	Channels    int
}

// DefaultPayloads returns the source-defined payload table used by fixtures.
func DefaultPayloads() map[uint8]PayloadSpec {
	l16 := PayloadSpec{PayloadType: 96, Format: PayloadL16, ClockRate: 48000, Channels: 2}
	h264 := PayloadSpec{PayloadType: 97, Format: PayloadH264, ClockRate: 90000}
	return map[uint8]PayloadSpec{l16.PayloadType: l16, h264.PayloadType: h264}
}

// SupportedPayloadFormats is useful for stable help/report output.
func SupportedPayloadFormats() []PayloadFormat {
	return []PayloadFormat{PayloadL16, PayloadH264}
}

// Packet is a parsed RTP datagram plus extension and provenance metadata.
type Packet struct {
	Packet       rtp.Packet
	Raw          []byte
	RecordIndex  int
	ExtSequence  uint64
	ExtTimestamp uint64
}

func (p *Packet) Sequence() uint16  { return p.Packet.SequenceNumber }
func (p *Packet) Timestamp() uint32 { return p.Packet.Timestamp }
func (p *Packet) SSRC() uint32      { return p.Packet.SSRC }

// ParseRTP parses one validated length-prefixed RTP record with pion/rtp.
func ParseRTP(rec Record) (*Packet, error) {
	if rec.Type != RecordRTP {
		return nil, media.Corrupt(media.StageRTP, "record type %d is not RTP", rec.Type)
	}
	var rp rtp.Packet
	if err := rp.Unmarshal(rec.Payload); err != nil {
		seq := sequenceFromRaw(rec.Payload)
		cerr := media.CorruptWrap(media.StageRTP, err, "invalid RTP packet")
		cerr = media.WithRecordIndex(cerr, rec.Index)
		if seq != nil {
			cerr = media.WithPacketSequence(cerr, *seq)
		}
		return nil, cerr
	}
	return &Packet{
		Packet:      rp,
		Raw:         append([]byte(nil), rec.Payload...),
		RecordIndex: rec.Index,
	}, nil
}

// ExtendUint16 unwraps a 16-bit modulo counter into a monotonic 64-bit value.
// prev is the previously emitted extended value; initialized=false starts at v.
func ExtendUint16(prev uint64, v uint16, initialized bool) uint64 {
	return extendModulo(prev, uint64(v), 16, initialized)
}

// ExtendUint32 unwraps a 32-bit modulo counter into a monotonic 64-bit value.
func ExtendUint32(prev uint64, v uint32, initialized bool) uint64 {
	return extendModulo(prev, uint64(v), 32, initialized)
}

func extendModulo(prev, v uint64, bits uint, initialized bool) uint64 {
	if !initialized {
		return v
	}
	mod := uint64(1) << bits
	mask := mod - 1
	half := mod >> 1
	base := prev &^ mask
	candidate := base | (v & mask)
	// A large backward jump around zero is actually forward wrap, and vice
	// versa. The half-window rule matches RTP sequence/timestamp practice.
	if candidate < prev && prev-candidate > half {
		candidate += mod
	} else if candidate > prev && candidate-prev > half {
		candidate -= mod
	}
	return candidate
}

// L16SampleCount returns the number of complete interleaved frames in an L16
// payload for the configured channel count.
func L16SampleCount(payload []byte, channels int) (int, error) {
	if channels <= 0 {
		return 0, media.Internal(media.StageAudio, "invalid L16 channel count %d", channels)
	}
	frameBytes := channels * 2
	if frameBytes == 0 || len(payload)%frameBytes != 0 {
		return 0, media.Corrupt(media.StageRTP, "L16 payload length %d is not a whole %d-channel frame", len(payload), channels)
	}
	return len(payload) / frameBytes, nil
}
