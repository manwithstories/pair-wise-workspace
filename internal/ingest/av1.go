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

// av1Keyframe walks the OBUs of a low-overhead temporal unit (one start
// code followed by concatenated OBUs) and reports a keyframe: the unit must
// carry both a sequence header and a KEY_FRAME frame header.
func av1Keyframe(payload []byte) bool {
	sc := findStartCode(payload, 0)
	if sc != 0 {
		return false
	}
	pos := startCodeLen(payload, sc)
	hasSeq, hasKey := false, false
	for pos < len(payload) {
		hdr := payload[pos]
		t := av1OBUType(hdr)
		pos++
		size := len(payload) - pos
		if hdr&0x02 != 0 { // obu_has_size_field
			v, n := readLEB128(payload[pos:])
			if n <= 0 {
				return false
			}
			pos += n
			size = int(v)
		}
		if pos+size > len(payload) {
			return false
		}
		obu := payload[pos : pos+size]
		switch t {
		case obuTypeSequenceHeader:
			hasSeq = true
		case obuTypeFrameHeader, obuTypeFrame:
			if frameHeaderIsKey(obu) {
				hasKey = true
			}
		}
		pos += size
	}
	return hasSeq && hasKey
}

// frameHeaderIsKey reads the show_existing_frame / frame_type bits from the
// frame header OBU's payload (header and size already stripped).
func frameHeaderIsKey(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	// 3-bit show_existing_frame at the top, then 2-bit frame_type. With the
	// synthetic minimal header the low two bits hold frame_type directly.
	if payload[0]&0x04 != 0 { // show_existing_frame == 1
		return false
	}
	return payload[0]&0x03 == 0
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
