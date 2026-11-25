package codec

import (
	"mediad/internal/timeline"
)

const (
	OBUSequenceHeader = 1
	OBUFrameHeader    = 3
	OBUFrame          = 6
)

// EncodeLEB128 encodes n in unsigned LEB128.
func EncodeLEB128(n uint64) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

func readLEB128(b []byte) (uint64, int) {
	var v uint64
	var shift uint
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if i == 9 && c > 1 { // keep uint64 valid
			return 0, 0
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
	}
	return 0, 0
}

func parseAV1(au []byte, claimedKey bool) boundaryInfo {
	info := boundaryInfo{errAt: -1}
	pos := 0
	frameOBUs := 0
	for pos < len(au) {
		headerAt := pos
		header := au[pos]
		pos++
		if header&0x80 != 0 {
			info.errAt = headerAt
			info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1,
				"forbidden OBU bit set")
			return info
		}
		obuType := int((header >> 3) & 0x0f)
		ext := header&0x04 != 0
		hasSize := header&0x02 != 0
		if ext {
			if pos >= len(au) {
				info.errAt = pos
				info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1,
					"truncated OBU extension")
				return info
			}
			pos++
		}
		if !hasSize {
			info.errAt = headerAt
			info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1,
				"fixture AV1 OBU must carry length")
			return info
		}
		size, n := readLEB128(au[pos:])
		if n == 0 {
			if pos >= len(au) {
				info.errAt = len(au)
			} else {
				info.errAt = pos
			}
			info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1,
				"invalid OBU length LEB128")
			return info
		}
		sizeAt := pos
		pos += n
		end := pos + int(size)
		if end > len(au) || end < pos {
			info.errAt = len(au) // first unavailable byte
			info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1,
				"OBU type=%d length=%d ends beyond AU at LEB128 offset %d", obuType, size, sizeAt)
			return info
		}
		info.units++
		if obuType == OBUFrame || obuType == OBUFrameHeader {
			frameOBUs++
			if len(au[pos:end]) == 0 {
				info.errAt = pos
				info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1,
					"empty frame OBU")
				return info
			}
			payload := au[pos:end]
			if payload[0]&0x80 == 0 { // show_existing_frame == 0
				frameType := (payload[0] >> 5) & 0x03
				if frameType == 0 { // KEY_FRAME
					info.keyframe = true
				}
			}
		}
		pos = end
	}
	if info.units == 0 {
		info.errAt = 0
		info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1, "empty AV1 access unit")
		return info
	}
	if frameOBUs == 0 {
		info.errAt = 0
		info.err = timeline.New(timeline.KindUnrecoverable, "codec.av1", -1, -1,
			"AV1 access unit contains no frame OBU")
		return info
	}
	if err := checkKeyClaim("av1", claimedKey, info.keyframe, 0, 0, &info); err != nil {
		return info
	}
	return info
}

// ExtractAV1Config returns sequence-header OBUs.
func ExtractAV1Config(au []byte) []byte {
	pos := 0
	var out []byte
	for pos < len(au) {
		start := pos
		header := au[pos]
		pos++
		obuType := int((header >> 3) & 0x0f)
		if header&0x04 != 0 {
			pos++
		}
		size, n := readLEB128(au[pos:])
		if n == 0 {
			return out
		}
		pos += n
		end := pos + int(size)
		if end > len(au) {
			return out
		}
		if obuType == OBUSequenceHeader {
			out = append(out, au[start:end]...)
		}
		pos = end
	}
	return out
}
