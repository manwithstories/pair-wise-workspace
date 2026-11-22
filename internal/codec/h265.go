package codec

import (
	"io"

	"mediapack/internal/fault"
)

// H.265 NAL unit types (ISO/IEC 23008-2, Table 7-1).
const (
	h265IDR_W   = 19
	h265IDR_N   = 20
	h265VPS     = 32
	h265SPS     = 33
	h265PPS     = 34
	h265AUDType = 35
)

// parseH265 frames an H.265 (HEVC) Annex B elementary stream.
func parseH265(r io.Reader, trackID uint32) ([]AccessUnit, *StreamMeta, error) {
	return parseAnnexB(r, trackID, H265, h265Classify, h265IsAUD)
}

func h265Classify(raw []byte) (nalType uint8, key, config bool, err error) {
	t := (raw[0] >> 1) & 0x3F
	// The AUD unit carries only its single-byte header in the synthetic stream.
	if t != h265AUDType && len(raw) < 2 {
		return 0, false, false, fault.New(fault.KindCorruptInput, "parse",
			"H.265 NAL header needs two bytes")
	}
	if raw[0]&0x80 != 0 {
		return 0, false, false, fault.New(fault.KindCorruptInput, "parse",
			"H.265 forbidden_zero_bit is set")
	}
	switch t {
	case h265IDR_W, h265IDR_N:
		key = true
	case h265VPS, h265SPS, h265PPS:
		config = true
	}
	return t, key, config, nil
}

func h265IsAUD(t uint8) bool { return t == h265AUDType }
