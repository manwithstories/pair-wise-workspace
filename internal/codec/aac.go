package codec

// AACParserV1 validates ADTS frame headers and frame boundaries.
type AACParserV1 struct{}

// NewAACParserV1 constructs the version-one AAC parser.
func NewAACParserV1() *AACParserV1 { return &AACParserV1{} }

// Capabilities implements ParserV1.
func (p *AACParserV1) Capabilities() Capabilities {
	return Capabilities{Codec: CodecAAC, ParserVersion: 1, Input: "adts", ADTS: true}
}

var adtsSampleRates = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

// Parse validates one or more ADTS AAC frames in a PES payload.
func (p *AACParserV1) Parse(payload []byte, ctx ParseContext) ([]Sample, error) {
	if len(payload) < 7 {
		return nil, corrupt(ctx, "AAC payload shorter than ADTS header")
	}
	var samples []Sample
	off := 0
	pts := ctx.PTS
	for off < len(payload) {
		if len(payload)-off < 7 {
			return nil, corrupt(ctx, "truncated ADTS header at payload offset %d", off)
		}
		h := payload[off:]
		if h[0] != 0xff || h[1]&0xf0 != 0xf0 {
			return nil, corrupt(ctx, "invalid ADTS sync 0x%02x%02x", h[0], h[1])
		}
		protectionAbsent := h[1] & 0x01
		headerLen := 7
		if protectionAbsent == 0 {
			headerLen = 9
		}
		if len(h) < headerLen {
			return nil, corrupt(ctx, "ADTS CRC header truncated")
		}
		profile := (h[2] >> 6) & 0x03 // AAC object type minus one
		rateIndex := (h[2] >> 2) & 0x0f
		channels := int((h[2]&0x01)<<2 | (h[3] >> 6))
		frameLen := int(uint16(h[3]&0x03)<<11 | uint16(h[4])<<3 | uint16(h[5]>>5))
		if frameLen < headerLen || off+frameLen > len(payload) {
			return nil, corrupt(ctx, "ADTS frame length %d exceeds payload at offset %d", frameLen, off)
		}
		rate := 0
		if rateIndex < 15 {
			rate = adtsSampleRates[rateIndex]
		}
		if rate == 0 || channels == 0 || profile > 3 {
			return nil, corrupt(ctx, "unsupported ADTS profile=%d rate_index=%d channels=%d", profile, rateIndex, channels)
		}
		frame := payload[off : off+frameLen]
		samples = append(samples, Sample{
			Codec: CodecAAC, PTS: pts, DTS: pts, Payload: frame,
			SampleRate: rate, Channels: channels,
		})
		// Fixture/demo uses one ADTS frame per PES; when multiple are present,
		// advance by the canonical 1024-sample AAC duration at the parsed rate.
		pts += int64(1024 * 90000 / rate)
		off += frameLen
	}
	if len(samples) == 0 {
		return nil, corrupt(ctx, "AAC payload contains no frames")
	}
	return samples, nil
}
