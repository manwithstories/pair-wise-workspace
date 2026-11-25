package ingest

import "github.com/fieldcut/fieldcut/internal/base"

// H.264 NAL unit type constants (ITU-T H.264 Table 7-1).
const (
	h264NALUnspecified = 0
	h264NALNonIDR      = 1
	h264NALIDR         = 5
	h264NALSEI         = 6
	h264NALSPS         = 7
	h264NALPPS         = 8
	h264NALMax         = 31
)

func h264NALType(b byte) uint8 { return b & 0x1f }

// h264Keyframe reports whether an Annex B access unit contains an IDR slice
// (parameter-set units alone do not count as a random-access frame).
func h264Keyframe(payload []byte) bool {
	i := 0
	for i < len(payload) {
		sc := findStartCode(payload, i)
		if sc < 0 {
			return false
		}
		off := sc + 3
		if sc+4 < len(payload) && payload[sc+2] == 0 && payload[sc+3] == 1 {
			off = sc + 4
		}
		if off >= len(payload) {
			return false
		}
		if h264NALType(payload[off]) == h264NALIDR {
			return true
		}
		i = off
	}
	return false
}

// ScanH264 scans an H.264 Annex B bare stream against its manifest.
func ScanH264(m *Manifest, data []byte, clock *base.Clock, sink FaultSink) ([]*AccessUnit, ScanStats, error) {
	if m.Codec != CodecH264 {
		return nil, ScanStats{}, base.NewFault(base.FaultIndexDrift,
			base.Locator{Camera: m.Camera, Rendition: m.Rendition}, false,
			"codec mismatch: expected h264, manifest says %s", m.Codec)
	}
	return scanStream(m, data, clock, sink, func(c Codec, p []byte) bool {
		return h264Keyframe(p)
	})
}
