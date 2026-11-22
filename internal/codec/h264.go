package codec

import (
	"io"

	"mediapack/internal/fault"
)

// H.264 NAL unit types (ISO/IEC 14496-10, Table 7-1).
const (
	h264NonIDR  = 1
	h264IDR     = 5
	h264SEI     = 6
	h264SPS     = 7
	h264PPS     = 8
	h264AUDType = 9
)

// parseH264 frames an H.264 Annex B elementary stream into access units.
// Timestamps are assigned monotonically on the shared 90kHz RTP clock at
// 3000 ticks per frame; wraparound is a transport concern handled later.
func parseH264(r io.Reader, trackID uint32) ([]AccessUnit, *StreamMeta, error) {
	return parseAnnexB(r, trackID, H264, h264Classify, h264IsAUD)
}

func h264Type(header byte) uint8 { return header & 0x1F }

func h264Classify(raw []byte) (nalType uint8, key, config bool, err error) {
	if len(raw) == 0 {
		return 0, false, false, fault.New(fault.KindCorruptInput, "parse", "zero-length H.264 NAL")
	}
	if raw[0]&0x80 != 0 {
		return 0, false, false, fault.At(fault.KindCorruptInput, "parse", -1,
			"H.264 forbidden_zero_bit is set")
	}
	t := h264Type(raw[0])
	switch t {
	case h264IDR:
		key = true
	case h264SPS, h264PPS:
		config = true
	}
	return t, key, config, nil
}

func h264IsAUD(t uint8) bool { return t == h264AUDType }
