package codec

import (
	"bytes"

	"mediapack/internal/fault"
)

// frameDuration is the fixed per-frame delta on the 90kHz clock (30 fps).
const frameDuration = 3000

// ParseBytes parses a buffered elementary stream, auto-detecting its codec
// from the first framed unit, and returns every access unit plus metadata.
// Inputs are bounded by the pipeline's configured size limit, so holding one
// copy of the bytes is acceptable.
func ParseBytes(data []byte, trackID uint32) ([]AccessUnit, *StreamMeta, error) {
	c, err := DetectCodec(data)
	if err != nil {
		return nil, nil, err
	}
	r := bytes.NewReader(data)
	switch c {
	case H264:
		return parseH264(r, trackID)
	case H265:
		return parseH265(r, trackID)
	default:
		return parseAV1(r, trackID)
	}
}

// DetectCodec identifies the bitstream from its first framed unit header.
func DetectCodec(data []byte) (Codec, error) {
	sc := newAnnexBScanner(bytes.NewReader(data), MaxNALBytes)
	first, _, err := sc.next()
	if err != nil {
		return 0, err
	}
	if len(first) == 0 {
		return 0, fault.New(fault.KindCorruptInput, "parse", "empty first unit")
	}
	return detectCodec(first)
}

func detectCodec(first []byte) (Codec, error) {
	h := first[0]
	switch {
	case h&0x1F == h264AUDType:
		return H264, nil
	case (h>>1)&0x3F == h265AUDType:
		return H265, nil
	case av1Type(h) == av1OBUTemporalDel || av1Type(h) == av1OBUSeqHeader:
		return AV1, nil
	}
	// Fallback heuristics for streams without a leading delimiter. The
	// forbidden bit is intentionally not checked here: the parser validates
	// it and must retain the precise input offset for fault reporting.
	switch {
	case h&0x02 == 0 && h&0x04 != 0 && av1Type(h) <= av1OBUPadding:
		return AV1, nil
	case (h>>1)&0x3F <= h265AUDType:
		return H265, nil
	default:
		return H264, nil
	}
}
