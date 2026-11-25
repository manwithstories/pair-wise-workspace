package ingest

import "github.com/fieldcut/fieldcut/internal/base"

// H.265 HEVC NAL unit types (ITU-T H.265 Table 7-1).
const (
	h265NALTRAIL_N    = 0
	h265NALIDR_W_RADL = 19
	h265NALIDR_N_LP   = 20
	h265NALCRA        = 21
	h265NALVPS        = 32
	h265NALSPS        = 33
	h265NALPPS        = 34
	h265NALMax        = 63
)

// h265NALType extracts the six-bit nal_unit_type from the header byte pair.
func h265NALType(first byte) uint8 { return (first >> 1) & 0x3f }

// h265Keyframe reports an IRAP access unit: IDR or CRA slices.
func h265Keyframe(payload []byte) bool {
	i := 0
	for i < len(payload) {
		sc := findStartCode(payload, i)
		if sc < 0 {
			return false
		}
		off := sc + startCodeLen(payload, sc)
		if off >= len(payload) {
			return false
		}
		switch h265NALType(payload[off]) {
		case h265NALIDR_W_RADL, h265NALIDR_N_LP, h265NALCRA:
			return true
		}
		i = off
	}
	return false
}

// ScanH265 scans an H.265 Annex B bare stream against its manifest.
func ScanH265(m *Manifest, data []byte, clock *base.Clock, sink FaultSink) ([]*AccessUnit, ScanStats, error) {
	if m.Codec != CodecH265 {
		return nil, ScanStats{}, base.NewFault(base.FaultIndexDrift,
			base.Locator{Camera: m.Camera, Rendition: m.Rendition}, false,
			"codec mismatch: expected h265, manifest says %s", m.Codec)
	}
	return scanStream(m, data, clock, sink, func(c Codec, p []byte) bool {
		return h265Keyframe(p)
	})
}
