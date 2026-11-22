package adapter

import (
	"encoding/binary"

	"mediaroute/internal/fault"
)

// SEI NAL conventions for the synthetic pipeline: forbidden_zero(1)
// nal_ref_idc(2) nal_unit_type(5) header byte, type 6 = SEI. Each SEI
// message is payloadType(255-extended) payloadSize(255-extended) payload,
// terminated by 0x80. Payload type 5 (user_data_unregistered) carries an
// 8-byte big-endian nanosecond timestamp followed by free-form data.
const (
	seiNALType          = 6
	seiUserDataUnreg    = 5
	seiTrailingByte     = 0x80
)

// SEIParser decodes SEI NAL units into unified events.
type SEIParser struct{}

// NewSEIParser returns a stateless SEI parser.
func NewSEIParser() *SEIParser { return &SEIParser{} }

// MajorVersion implements Parser.
func (p *SEIParser) MajorVersion() int { return 1 }

// Parse decodes one SEI NAL unit.
func (p *SEIParser) Parse(track string, data []byte, offset int64) ([]Event, error) {
	if len(data) < 2 {
		return nil, fault.Newf(fault.CatParse, "sei: short nal unit (%d bytes)", len(data)).
			WithTrack(track).WithOffset(offset)
	}
	if data[0]&0x1f != seiNALType {
		return nil, fault.Newf(fault.CatParse, "sei: nal type %d, want 6", data[0]&0x1f).
			WithTrack(track).WithOffset(offset)
	}
	var events []Event
	i := 1
	for i < len(data) && data[i] != seiTrailingByte {
		pt, n := readSEIScalar(data[i:])
		if n == 0 {
			return events, fault.New(fault.CatParse, "sei: truncated payload type").
				WithTrack(track).WithOffset(offset + int64(i))
		}
		i += n
		size, n := readSEIScalar(data[i:])
		if n == 0 {
			return events, fault.New(fault.CatParse, "sei: truncated payload size").
				WithTrack(track).WithOffset(offset + int64(i))
		}
		i += n
		if i+size > len(data) {
			return events, fault.Newf(fault.CatParse,
				"sei: truncated payload: need %d have %d", size, len(data)-i).
				WithTrack(track).WithOffset(offset + int64(i))
		}
		payload := data[i : i+size]
		i += size
		if pt != seiUserDataUnreg {
			continue
		}
		if len(payload) < 8 {
			return events, fault.New(fault.CatParse, "sei: user data too short for timestamp").
				WithTrack(track).WithOffset(offset + int64(i))
		}
		events = append(events, Event{
			Track:       track,
			Kind:        KindSEI,
			TimestampNs: int64(binary.BigEndian.Uint64(payload[:8])),
			Payload:     payload[8:],
		})
	}
	return events, nil
}

// readSEIScalar decodes a 255-extended SEI scalar; returns (value, bytes)
// or (0, 0) when truncated.
func readSEIScalar(b []byte) (int, int) {
	v, n := 0, 0
	for n < len(b) {
		v += int(b[n])
		n++
		if b[n-1] != 0xff {
			return v, n
		}
	}
	return 0, 0
}

// BuildSEI assembles a synthetic SEI NAL unit; used by the simulator.
func BuildSEI(tsNs int64, data []byte) []byte {
	payload := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(payload, uint64(tsNs))
	copy(payload[8:], data)
	out := []byte{seiNALType, seiUserDataUnreg, byte(len(payload))}
	out = append(out, payload...)
	out = append(out, seiTrailingByte)
	return out
}
