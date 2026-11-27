package metadata

import (
	"encoding/binary"

	"fastpost/internal/model"
)

// timingPayloadType is the payload type of the drill's custom
// frame-timing SEI message (user-data style, fixed binary layout).
const timingPayloadType uint32 = 0x67 // 103, outside the H.264 reserved range

// trailingMarker is the synthetic rbsp_trailing marker used by the
// fixtures to terminate an SEI payload region cleanly.
const trailingMarker uint32 = 0x80

// SEITiming is one decoded frame-timing SEI message.
type SEITiming struct {
	Event    TimedEvent
	FrameNum int32
	Skew     int64
}

// splitNALs splits an Annex-B stream into NAL units, returning each unit
// with the absolute byte offset where its start code begins.
func splitNALs(data []byte) ([]nalUnit, []int64) {
	var units []nalUnit
	var starts []int64
	// Locate every start code with a single linear scan. Leading zeros
	// belong to the start code, never to the preceding NAL unit.
	type sc struct {
		start int
		body  int
	}
	var codes []sc
	i := 0
	for i+2 < len(data) {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				codes = append(codes, sc{i, i + 3})
				i += 3
				continue
			}
			if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				codes = append(codes, sc{i, i + 4})
				i += 4
				continue
			}
		}
		i++
	}
	for k, c := range codes {
		end := len(data)
		if k+1 < len(codes) {
			end = codes[k+1].start
		}
		// headerOff is the absolute offset of the NAL header byte, i.e.
		// the first byte after the start code.
		units = append(units, nalUnit{headerOff: int64(c.body), rbsp: data[c.body:end]})
		starts = append(starts, int64(c.start))
	}
	return units, starts
}

type nalUnit struct {
	headerOff int64
	rbsp      []byte
}

// isSEI reports whether an RBSP starts with an SEI NAL header byte for
// H.264 (6) or H.265 (39 prefix / 40 suffix).
func isSEI(rbsp []byte) bool {
	if len(rbsp) == 0 {
		return false
	}
	h := rbsp[0] & 0x1f
	return h == 6 || h == 39 || h == 40
}

// ParseSEI walks every SEI NAL unit in an Annex-B stream and decodes the
// custom frame-timing messages. Truncation faults name the absolute byte
// offset of the first byte the parser could not consume.
func ParseSEI(track string, data []byte, rate RationalLike) ([]SEITiming, error) {
	col := &model.Collector{}
	units, _ := splitNALs(data)
	var out []SEITiming
	foundOne := false
	for _, u := range units {
		if !isSEI(u.rbsp) {
			continue
		}
		payload := nalEmulationPrevention(u.rbsp[1:])
		// payload[0] sits one byte after the NAL header byte, so its
		// absolute offset is headerOff+1. All offsets reported below are
		// base plus the index inside this payload slice.
		off := int(u.headerOff) + 1
		t, ok := parseSEIMessages(track, payload, off, rate, col)
		if !ok {
			return out, col.Err()
		}
		out = append(out, t...)
		foundOne = true
	}
	if !foundOne && len(data) > 0 {
		col.Add(model.FaultSEITruncated, track, 0, "no SEI NAL unit found in stream")
	}
	return out, col.Err()
}

// parseSEIMessages walks the payloadType/payloadSize framing of one SEI
// NAL unit. It stops and reports the first malformed offset.
func parseSEIMessages(track string, payload []byte, base int, rate RationalLike, col *model.Collector) ([]SEITiming, bool) {
	var out []SEITiming
	i := 0
	for i < len(payload) {
		pType, n, ok := readSEILen(payload, i)
		if !ok {
			col.Add(model.FaultSEITruncated, track, int64(base+i), "truncated payload_type")
			return out, false
		}
		// The trailing marker is a single byte with no following size, so
		// it must be recognized before attempting to read payload_size.
		if pType == trailingMarker {
			return out, true
		}
		i = n
		pSize, n, ok := readSEILen(payload, i)
		if !ok {
			col.Add(model.FaultSEITruncated, track, int64(base+i), "truncated payload_size")
			return out, false
		}
		i = n
		if i+int(pSize) > len(payload) {
			// The first byte the payload_size claim needs but the blob
			// does not contain: report that exact offset.
			missing := base + len(payload)
			col.Add(model.FaultSEITruncated, track, int64(missing),
				"payload extends beyond NAL unit; first missing byte")
			return out, false
		}
		body := payload[i : i+int(pSize)]
		if pType == timingPayloadType {
			t, ok := parseTimingPayload(track, body, base+i, rate, col)
			if !ok {
				return out, false
			}
			out = append(out, t)
		}
		i += int(pSize)
	}
	return out, true
}

// readSEILen decodes the multi-byte 0xFF-prefixed SEI length encoding.
func readSEILen(b []byte, i int) (uint32, int, bool) {
	var v uint32
	for i < len(b) {
		c := b[i]
		i++
		v += uint32(c)
		if c != 0xFF {
			return v, i, true
		}
	}
	return 0, i, false
}

// minTimingBody is the number of leading bytes that carry structured
// timing fields; any trailing bytes are opaque padding.
const minTimingBody = 16

// parseTimingPayload decodes the custom timing layout: frameNum uint32,
// skew uint32, position uint64 (big-endian), followed by optional padding.
func parseTimingPayload(track string, body []byte, off int, rate RationalLike, col *model.Collector) (SEITiming, bool) {
	if len(body) < minTimingBody {
		col.Add(model.FaultSEITruncated, track, int64(off+len(body)),
			"timing payload truncated; first missing byte")
		return SEITiming{}, false
	}
	frame := int32(binary.BigEndian.Uint32(body[0:4]))
	skew := int64(int32(binary.BigEndian.Uint32(body[4:8])))
	pos := int64(binary.BigEndian.Uint64(body[8:16]))
	if pos < 0 {
		col.Add(model.FaultUnmappableEvent, track, int64(off+12), "negative frame position")
		return SEITiming{}, false
	}
	return SEITiming{
		FrameNum: frame,
		Skew:     skew,
		Event: TimedEvent{
			Kind:     KindSEI,
			Track:    track,
			Offset:   int64(off),
			Position: pos,
			RateNum:  rate.Num,
			RateDen:  rate.Den,
			ID:       seiID(frame),
			Payload:  append([]byte(nil), body...),
		},
	}, true
}

func seiID(frame int32) string {
	return "sei-frame-" + itoa64(int64(frame))
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// RationalLike is the rate pair the metadata package accepts without
// importing the timeline package (avoids an import cycle in fixtures).
type RationalLike struct {
	Num int64
	Den int64
}
