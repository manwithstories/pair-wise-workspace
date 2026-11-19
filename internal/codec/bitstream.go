package codec

// This file implements the minimal bitstream probes needed to confirm that an
// index entry really points at a sync sample. It deliberately parses only the
// headers, never payload, so a corrupt frame body cannot mask a bad index.

// Annex-B start codes: three-byte 0x000001 or four-byte 0x00000001.
func findStartCode(b []byte, from int) (codeAt, nalAt int) {
	for i := from; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				return i, i + 3
			}
			if b[i+2] == 0 && i+4 <= len(b) && b[i+3] == 1 {
				return i, i + 4
			}
		}
	}
	return -1, -1
}

// h264NaluTypes returns the NAL unit types found in an Annex-B buffer.
func h264NaluTypes(b []byte) []byte {
	var types []byte
	_, nal := findStartCode(b, 0)
	if nal < 0 {
		return nil
	}
	for nal >= 0 && nal < len(b) {
		nextCode, nextNal := findStartCode(b, nal)
		end := len(b)
		if nextCode >= 0 {
			end = nextCode
		}
		if end > nal {
			types = append(types, b[nal]&0x1f)
		}
		nal = nextNal
	}
	return types
}

// h264HasIDR reports whether the buffer contains an IDR slice (type 5).
func h264HasIDR(b []byte) bool {
	for _, t := range h264NaluTypes(b) {
		if t == 5 {
			return true
		}
	}
	return false
}

// h265HasIRAP reports whether the buffer contains an IRAP picture. In H.265
// the NAL type occupies bits 1..6 of the first header byte.
func h265HasIRAP(b []byte) bool {
	_, nal := findStartCode(b, 0)
	for nal >= 0 && nal < len(b) {
		nextCode, nextNal := findStartCode(b, nal)
		end := len(b)
		if nextCode >= 0 {
			end = nextCode
		}
		if end > nal {
			t := (b[nal] >> 1) & 0x3f
			// 16..23 are BLA/IDR/CRA pictures; 19/20 are the common IDRs.
			if t >= 16 && t <= 23 {
				return true
			}
		}
		nal = nextNal
	}
	return false
}

// AV1 OBU types used by the probe.
const (
	obuSequenceHeader = 1
	obuTemporalDelim  = 2
	obuFrameHeader    = 3
	obuFrame          = 6
)

// av1IsKeyframe scans AV1 OBUs and reports whether the first coded frame is a
// KEY_FRAME. The OBU header is: forbidden(1) type(4) extension(1) has_size(1)
// reserved(1). The frame header starts with show_existing_frame(1), then
// frame_type(2) when show_existing_frame is zero.
func av1IsKeyframe(b []byte) bool {
	off := 0
	sawSequence := false
	for off < len(b) {
		hdr := b[off]
		obuType := (hdr >> 3) & 0x0f
		ext := hdr&0x04 != 0
		hasSize := hdr&0x02 != 0
		off++
		if ext {
			off++ // temporal/spatial layer byte
		}
		if !hasSize {
			// Only a temporal delimiter may omit the size field; anything else
			// is not safely walkable, so stop rather than guess.
			if obuType == obuTemporalDelim {
				continue
			}
			return false
		}
		size, n, ok := leb128(b[off:])
		if !ok {
			return false
		}
		off += n
		if size > uint64(len(b)-off) {
			return false
		}
		payload := b[off : off+int(size)]
		switch obuType {
		case obuSequenceHeader:
			sawSequence = true
		case obuFrame, obuFrameHeader:
			if !sawSequence {
				// A frame without its sequence header is not decodable; a
				// keyframe claim here cannot be confirmed.
				return false
			}
			return av1FrameIsKey(payload)
		}
		off += int(size)
	}
	return false
}

// av1FrameIsKey reads show_existing_frame and frame_type from an uncompressed
// frame header.
func av1FrameIsKey(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if payload[0]&0x80 != 0 {
		// show_existing_frame: no new frame type to trust.
		return false
	}
	if len(payload) < 1 {
		return false
	}
	frameType := (payload[0] >> 5) & 0x03
	return frameType == 0
}

// leb128 decodes an AV1 OBU size field.
func leb128(b []byte) (uint64, int, bool) {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}
