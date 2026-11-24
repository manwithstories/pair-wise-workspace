package testkit

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/pion/rtp"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// RTPPacket is one wire-ready packet with the metadata the receiver needs.
type RTPPacket struct {
	TrackID   uint32
	Sequence  uint16
	Timestamp uint32
	Marker    bool
	Payload   []byte
}

// EncodeWire marshals an RTP packet with pion/rtp.
func EncodeWire(p RTPPacket) ([]byte, error) {
	if len(p.Payload) > 2000 {
		return nil, fmt.Errorf("%w: rtp payload %d", model.ErrLimit, len(p.Payload))
	}
	wire := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			SequenceNumber: p.Sequence,
			Timestamp:      p.Timestamp,
			Marker:         p.Marker,
		},
		Payload: p.Payload,
	}
	return wire.Marshal()
}

// GeneratePackets builds the packet stream for one track under a scenario.
// The returned packets are in the order they should be fed to the receiver;
// gaps and duplicates are part of the defect pattern, not of the payload.
func GeneratePackets(seed int64, scenario Scenario, trackID uint32, count int, startTick clock.Tick) ([]RTPPacket, error) {
	if count <= 0 || count > MaxPackets {
		return nil, fmt.Errorf("%w: packet count %d", model.ErrInvalid, count)
	}
	rng := rand.New(rand.NewSource(seed))
	packets := make([]RTPPacket, count)
	for i := 0; i < count; i++ {
		var ts uint32
		if scenario == ScenarioWrap {
			// Start close to the 32-bit boundary so the stream wraps.
			ts = uint32(0xFFFFFF00 + i*3000)
		} else {
			ts = uint32(int64(startTick)/(int64(clock.TickRate)/int64(90000))) + uint32(i*3000)
		}
		payload := make([]byte, 12)
		for j := range payload {
			payload[j] = byte(rng.Intn(256))
		}
		binary.BigEndian.PutUint32(payload[0:4], uint32(i))
		packets[i] = RTPPacket{
			TrackID:   trackID,
			Sequence:  uint16(i),
			Timestamp: ts,
			Marker:    i%5 == 0,
			Payload:   payload,
		}
	}
	return ReorderStream(packets, scenario, rng)
}

// ReorderStream applies the defect pattern to an ordered packet stream.
func ReorderStream(in []RTPPacket, scenario Scenario, rng *rand.Rand) ([]RTPPacket, error) {
	switch scenario {
	case ScenarioClean:
		return in, nil
	case ScenarioReorder:
		out := append([]RTPPacket(nil), in...)
		for i := 0; i+2 < len(out); i += 7 {
			out[i], out[i+2] = out[i+2], out[i]
		}
		return out, nil
	case ScenarioDuplicate:
		var out []RTPPacket
		for i, p := range in {
			out = append(out, p)
			if i%9 == 4 {
				out = append(out, p)
			}
		}
		return out, nil
	case ScenarioBurst:
		var out []RTPPacket
		for i := 0; i < len(in); i++ {
			// Drop a burst of 3 packets every 11.
			if i%11 == 5 {
				i += 2
				continue
			}
			out = append(out, in[i])
		}
		return out, nil
	case ScenarioWrap:
		return in, nil
	default:
		return nil, fmt.Errorf("%w: scenario %q", model.ErrInvalid, scenario)
	}
}
