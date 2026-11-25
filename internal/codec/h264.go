package codec

// H264ParserV1 parses Annex-B NAL framing and keyframe indicators. It does not
// transcode or reconstruct pixels; alignment only needs bounded syntax checks.
type H264ParserV1 struct{}

// NewH264ParserV1 constructs the version-one H.264 parser.
func NewH264ParserV1() *H264ParserV1 { return &H264ParserV1{} }

// Capabilities implements ParserV1.
func (p *H264ParserV1) Capabilities() Capabilities {
	return Capabilities{
		Codec:             CodecH264,
		ParserVersion:     1,
		Input:             "annexb",
		KeyframeDetection: true,
	}
}

// Parse validates NAL headers and detects IDR access units.
func (p *H264ParserV1) Parse(payload []byte, ctx ParseContext) ([]Sample, error) {
	nals, err := SplitAnnexBNALs(payload)
	if err != nil {
		return nil, corrupt(ctx, "%v", err)
	}
	if len(nals) == 0 {
		return nil, corrupt(ctx, "H.264 access unit contains no NAL units")
	}
	types := make([]uint8, 0, len(nals))
	key := false
	for _, nal := range nals {
		if len(nal) == 0 {
			return nil, corrupt(ctx, "empty H.264 NAL unit")
		}
		if nal[0]&0x80 != 0 {
			return nil, corrupt(ctx, "H.264 forbidden_zero_bit set in NAL header 0x%02x", nal[0])
		}
		nalType := nal[0] & 0x1f
		if nalType == 0 || nalType > 31 {
			return nil, corrupt(ctx, "invalid H.264 NAL type %d", nalType)
		}
		types = append(types, nalType)
		if nalType == 5 {
			key = true
		}
	}
	return []Sample{{
		Codec:    CodecH264,
		PTS:      ctx.PTS,
		DTS:      ctx.DTS,
		Keyframe: key,
		Payload:  payload,
		NALTypes: types,
	}}, nil
}

// SplitAnnexBNALs returns NAL RBSP bytes without start-code prefixes.
func SplitAnnexBNALs(b []byte) ([][]byte, error) {
	type hit struct{ start, prefix int }
	var starts []hit
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			starts = append(starts, hit{i + 3, 3})
			i += 2
		} else if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			starts = append(starts, hit{i + 4, 4})
			i += 3
		}
	}
	if len(starts) == 0 {
		return nil, errNoStartCode
	}
	out := make([][]byte, 0, len(starts))
	for i, h := range starts {
		end := len(b)
		if i+1 < len(starts) {
			end = starts[i+1].start - starts[i+1].prefix
		}
		if h.start > end {
			return nil, errOverlap
		}
		nal := b[h.start:end]
		// Remove trailing zero bytes that preceded the next start code.
		for len(nal) > 0 && nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1]
		}
		if len(nal) > 0 {
			out = append(out, nal)
		}
	}
	return out, nil
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

const (
	errNoStartCode simpleError = "missing Annex-B start code"
	errOverlap     simpleError = "invalid overlapping Annex-B start codes"
)
