package codec

import (
	"mediad/internal/timeline"
)

type nalSpec struct {
	name      string
	headerLen int
	nalType   func(firstHeaderByte byte, au []byte, headerOff int) int
	isVCL     func(nalType int) bool
	isKey     func(nalType int) bool
}

var h264Spec = nalSpec{
	name:      "h264",
	headerLen: 1,
	nalType:   func(b0 byte, _ []byte, _ int) int { return int(b0 & 0x1f) },
	isVCL:     func(t int) bool { return t >= 1 && t <= 5 },
	isKey:     func(t int) bool { return t == 5 }, // IDR
}

var h265Spec = nalSpec{
	name:      "h265",
	headerLen: 2,
	nalType: func(_ byte, au []byte, h int) int {
		return int((au[h] >> 1) & 0x3f)
	},
	isVCL: func(t int) bool { return t < 32 },
	isKey: func(t int) bool { return t == 19 || t == 20 || t == 21 },
}

func parseAnnexB(au []byte, claimedKey bool, spec nalSpec) boundaryInfo {
	headers := annexBHeaders(au)
	if len(headers) == 0 {
		return boundaryInfo{errAt: 0, err: timeline.New(timeline.KindUnrecoverable, "codec."+spec.name, -1, -1,
			"missing Annex B start code")}
	}
	if headers[0] != 3 && headers[0] != 4 {
		return boundaryInfo{units: len(headers), errAt: 0, err: timeline.New(
			timeline.KindUnrecoverable, "codec."+spec.name, -1, -1,
			"AU must begin with 00 00 01 or 00 00 00 01")}
	}

	info := boundaryInfo{units: len(headers), errAt: -1}
	vcl := 0
	keyHeader := -1
	for _, h := range headers {
		if h >= len(au) {
			info.errAt = h
			info.err = timeline.New(timeline.KindUnrecoverable, "codec."+spec.name, -1, -1,
				"truncated NAL header at %d", h)
			return info
		}
		if h+spec.headerLen > len(au) {
			info.errAt = h + 1
			if info.errAt > len(au) {
				info.errAt = len(au)
			}
			info.err = timeline.New(timeline.KindUnrecoverable, "codec."+spec.name, -1, -1,
				"truncated %d-byte NAL header", spec.headerLen)
			return info
		}
		t := spec.nalType(au[h], au, h)
		if spec.isVCL(t) {
			vcl++
		}
		if spec.isKey(t) {
			info.keyframe = true
			keyHeader = h
		}
	}
	if vcl == 0 {
		info.errAt = headers[0]
		info.err = timeline.New(timeline.KindUnrecoverable, "codec."+spec.name, -1, -1,
			"access unit contains no VCL NAL unit")
		return info
	}
	if err := checkKeyClaim(spec.name, claimedKey, info.keyframe, keyHeader, headers[0], &info); err != nil {
		return info
	}
	return info
}

func checkKeyClaim(name string, claimed, detected bool, keyHeader, firstHeader int, info *boundaryInfo) error {
	if claimed == detected {
		return nil
	}
	at := keyHeader
	if at < 0 {
		at = firstHeader
	}
	info.errAt = at
	if claimed && !detected {
		info.err = timeline.New(timeline.KindUnrecoverable, "codec."+name, -1, -1,
			"index marks keyframe but no IDR/key NAL boundary found")
	} else {
		info.err = timeline.New(timeline.KindUnrecoverable, "codec."+name, -1, -1,
			"index does not mark keyframe but key NAL boundary found")
	}
	return info.err
}

// annexBHeaders returns the byte offset immediately after each start code.
func annexBHeaders(b []byte) []int {
	var out []int
	for i := 0; i < len(b); {
		if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			out = append(out, i+4)
			i += 4
			continue
		}
		if i+3 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			out = append(out, i+3)
			i += 3
			continue
		}
		i++
	}
	return out
}

// ExtractAnnexBConfig returns parameter-set NAL bytes (with start codes).
func ExtractAnnexBConfig(codec Codec, au []byte) []byte {
	headers := annexBHeaders(au)
	var spec nalSpec
	switch codec {
	case H264:
		spec = h264Spec
	case H265:
		spec = h265Spec
	default:
		return nil
	}
	var out []byte
	for n, h := range headers {
		end := len(au)
		if n+1 < len(headers) {
			// Recover start-code offset: header follows 3 or 4 byte code.
			nextHeader := headers[n+1]
			if nextHeader >= 4 && au[nextHeader-4] == 0 && au[nextHeader-3] == 0 &&
				au[nextHeader-2] == 0 && au[nextHeader-1] == 1 {
				end = nextHeader - 4
			} else {
				end = nextHeader - 3
			}
		}
		if h >= len(au) || h+spec.headerLen > len(au) {
			continue
		}
		t := spec.nalType(au[h], au, h)
		keep := false
		if codec == H264 && (t == 7 || t == 8) { // SPS, PPS
			keep = true
		}
		if codec == H265 && (t == 32 || t == 33 || t == 34) { // VPS, SPS, PPS
			keep = true
		}
		if keep {
			start := h - 4
			if start < 0 || au[start] != 0 || au[start+1] != 0 || au[start+2] != 0 || au[start+3] != 1 {
				start = h - 3
			}
			out = append(out, au[start:end]...)
		}
	}
	return out
}
