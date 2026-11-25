package ingest

import "github.com/fieldcut/fieldcut/internal/base"

// AV1 OBU types (AV1 spec Table 6.1).
const (
	obuTypeSequenceHeader = 1
	obuTypeFrameHeader    = 3
	obuTypeFrame          = 6
	obuTypeReserved       = 15
)

// av1OBUType extracts the four-bit obu_type.
func av1OBUType(b byte) uint8 { return (b >> 3) & 0x0f }

// av1FrameIsKey reads the show_existing_frame / frame_type fields from a
// frame header OBU. frame_type == 0 is KEY_FRAME; the synthetic generator
// writes a minimal header so the bit layout is fixed.
func av1FrameIsKey(obu []byte) bool {
	// obu layout: header byte, optional leb128 size, then payload.
	if len(obu) < 3 {
		return false
	}
	pos := 1
	if obu[0]&0x02 != 0 { // obu_has_size_field
		_, n := readLEB128(obu[pos:])
		if n <= 0 {
			return false
		}
		pos += n
	}
	payload := obu[pos:]
	// payload: 3-bit show_existing_frame, then 2-bit frame_type.
	if len(payload) == 0 {
		return false
	}
	if payload[0]&0x04 != 0 { // show_existing_frame == 1
		return false
	}
	return (payload[0]>>1)&0x03 == 0
}

// readLEB128 decodes an unsigned LEB128 value, returning the value and the
// number of bytes consumed (0 on error).
func readLEB128(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 8; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// av1Keyframe walks the OBUs of a low-overhead bitstream format unit and
// reports a keyframe: a temporal unit carrying both a sequence header and a
// KEY_FRAME frame header.
func av1Keyframe(payload []byte) bool {
	hasSeq, hasKey := false, false
	i := 0
	for i < len(payload) {
		sc := findStartCode(payload, i)
		if sc < 0 {
			break
		}
		off := sc + 3
		if sc+4 < len(payload) && payload[sc+2] == 0 && payload[sc+3] == 1 {
			off = sc + 4
		}
		if off >= len(payload) {
			break
		}
		t := av1OBUType(payload[off])
		switch t {
		case obuTypeSequenceHeader:
			hasSeq = true
		case obuTypeFrameHeader, obuTypeFrame:
			if av1FrameIsKey(payload[off:]) {
				hasKey = true
			}
		}
		i = off
	}
	return hasSeq && hasKey
}

// ScanAV1 scans an AV1 low-overhead bitstream against its manifest.
func ScanAV1(m *Manifest, data []byte, clock *base.Clock, sink FaultSink) ([]*AccessUnit, ScanStats, error) {
	if m.Codec != CodecAV1 {
		return nil, ScanStats{}, base.NewFault(base.FaultIndexDrift,
			base.Locator{Camera: m.Camera, Rendition: m.Rendition}, false,
			"codec mismatch: expected av1, manifest says %s", m.Codec)
	}
	return scanStream(m, data, clock, sink, func(c Codec, p []byte) bool {
		return av1Keyframe(p)
	})
}
