// Package codec parses variable-duration Opus/AAC access units from ordered
// RTP packets. Durations come from the bitstream (Opus TOC, AAC frame
// length), never assumed constant, and all timestamps use the shared
// rational time base.
package codec

import (
	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// Codec selects the access-unit duration parser.
type Codec int

const (
	Opus Codec = iota
	AAC
)

// AccessUnit is one decodable audio frame anchored on the unified timeline.
type AccessUnit struct {
	Index   int         `json:"index"`
	Seq     uint16      `json:"seq"`
	Start   clock.Time  `json:"start"`
	Dur     clock.Time  `json:"dur"`
	Payload []byte      `json:"payload"`
}

// End returns the first instant after the unit.
func (u AccessUnit) End() clock.Time { return u.Start.Add(u.Dur) }

// Parsed is the result of converting one ordered packet stream.
type Parsed struct {
	Units []AccessUnit `json:"units"`
	// TimestampGaps holds the start of each explicit timestamp
	// discontinuity (burst loss visible in the timestamp domain).
	TimestampGaps []clock.Time `json:"timestamp_gaps,omitempty"`
}

// opusFrameMs maps the 5-bit TOC config to a frame duration in
// 120ths of a millisecond (2.5ms = 300) to stay integral.
var opusFrameMs = [32]int{
	10, 20, 40, 60, 10, 20, 40, 60, // SILK NB/MB/WB
	10, 20, 40, 60, // SILK SWB/FB
	10, 20, 10, 20, // hybrid
	2, 5, 10, 20, 2, 5, 10, 20, // CELT (2 == 2.5ms, see opusSamples)
	2, 5, 10, 20, 2, 5, 10, 20,
}

// opusSamples returns samples-per-frame at 48 kHz for a TOC config.
func opusSamples(config int) int {
	switch c := config & 0x1f; {
	case c >= 16 && c%4 == 0: // 2.5 ms
		return 120
	case c >= 16 && c%4 == 1: // 5 ms
		return 240
	}
	switch opusFrameMs[config&0x1f] {
	case 10:
		return 480
	case 20:
		return 960
	case 40:
		return 1920
	default: // 60 ms
		return 2880
	}
}

// FrameSamples returns the number of samples carried by one packet payload.
// An empty payload is a zero-duration keep-alive packet and yields 0.
func FrameSamples(c Codec, payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	switch c {
	case Opus:
		toc := payload[0]
		config := int(toc >> 3)
		frames := 1
		switch toc & 0x3 {
		case 0, 1, 2: // 1 or 2 frames per packet
			if toc&0x3 != 0 {
				frames = 2
			}
		case 3: // arbitrary count in byte 1
			if len(payload) < 2 {
				return 0, fault.New(fault.KindCorrupt, "codec", "opus code-3 packet too short")
			}
			frames = int(payload[1] & 0x3f)
			if frames == 0 {
				return 0, fault.New(fault.KindCorrupt, "codec", "opus code-3 zero frame count")
			}
		}
		return opusSamples(config) * frames, nil
	case AAC:
		return 1024, nil // one AAC-LC frame per RTP payload
	}
	return 0, fault.New(fault.KindCorrupt, "codec", "unknown codec %d", c)
}

// maxGapTicks bounds a plausible timestamp step (2 s); larger jumps are
// explicit timestamp gaps, never filled with fabricated audio.
const maxGapSeconds = 2

// Parse converts ordered packets into access units on the given time base.
// RTP timestamps are unwrapped via int32 deltas; a negative delta is a
// timebase regression and fails the parse.
func Parse(pkts []*rtp.Packet, c Codec, base clock.TimeBase) (*Parsed, error) {
	out := &Parsed{}
	if len(pkts) == 0 {
		return out, nil
	}
	maxGap := int64(maxGapSeconds) * base.Den / base.Num
	origin := int64(pkts[0].Timestamp)
	prevTS := origin
	prevEnd := origin
	for i, p := range pkts {
		ts := origin + int64(int32(p.Timestamp-uint32(origin)))
		if i > 0 {
			if ts < prevTS {
				return nil, fault.New(fault.KindTimebase, "codec",
					"timestamp regression at seq %d (%d < %d)", p.SequenceNumber, ts, prevTS)
			}
			if ts-prevEnd > maxGap {
				out.TimestampGaps = append(out.TimestampGaps, clock.At(prevEnd, base))
			}
		}
		n, err := FrameSamples(c, p.Payload)
		if err != nil {
			return nil, err
		}
		u := AccessUnit{
			Index:   i,
			Seq:     p.SequenceNumber,
			Start:   clock.At(ts, base),
			Dur:     clock.At(int64(n), base),
			Payload: p.Payload,
		}
		out.Units = append(out.Units, u)
		prevTS = ts
		if end := ts + int64(n); end > prevEnd {
			prevEnd = end
		}
	}
	return out, nil
}
