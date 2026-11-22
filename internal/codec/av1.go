package codec

import (
	"io"

	"mediapack/internal/fault"
)

// AV1 OBU types (AV1 spec, Table 6.1).
const (
	av1OBUSeqHeader   = 1
	av1OBUTemporalDel = 2
	av1OBUFrameHeader = 3
	av1OBUTileGroup   = 4
	av1OBUFrame       = 6
	av1OBUPadding     = 15
)

// parseAV1 frames an AV1 stream carried in Annex B start-code framing. Each
// OBU is one start-code-delimited unit; temporal-delimiter OBUs open AUs.
func parseAV1(r io.Reader, trackID uint32) ([]AccessUnit, *StreamMeta, error) {
	return parseAnnexB(r, trackID, AV1, av1Classify, av1IsAUD)
}

func av1Type(header byte) uint8 { return (header >> 3) & 0x0F }

func av1Classify(raw []byte) (nalType uint8, key, config bool, err error) {
	if len(raw) == 0 {
		return 0, false, false, fault.New(fault.KindCorruptInput, "parse",
			"zero-length AV1 OBU")
	}
	h := raw[0]
	if h&0x80 != 0 {
		return 0, false, false, fault.New(fault.KindCorruptInput, "parse",
			"AV1 obu_forbidden_bit is set")
	}
	if h&0x02 != 0 {
		return 0, false, false, fault.New(fault.KindCorruptInput, "parse",
			"AV1 extension OBUs are not supported")
	}
	t := av1Type(h)
	if t == av1OBUSeqHeader {
		config = true
	}
	// Synthetic frames carry a keyframe marker as the first payload byte:
	// 0x01 = key frame, 0x00 = inter frame. Real decoders are never invoked.
	if (t == av1OBUFrame || t == av1OBUFrameHeader) && len(raw) >= 2 {
		key = raw[1] == 0x01
	}
	return t, key, config, nil
}

func av1IsAUD(t uint8) bool { return t == av1OBUTemporalDel }
