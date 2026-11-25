package fixture

// Minimal, well-known Baseline-Profile H.264 parameter-set NAL units for a
// 320x180 test stream (the same SPS/PPS bytes x264 emits for that size). The
// pipeline never decodes imagery; it frames NAL units and carries SPS/PPS
// into the fMP4 avcC box, so fixed valid bytes are sufficient.

// annexBPrefix is the 4-byte start code used before every NAL unit.
var annexBPrefix = []byte{0, 0, 0, 1}

var rawSPS = []byte{
	0x67, 0x42, 0x00, 0x14, 0xe9, 0x40, 0x40, 0x04,
	0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0xc8, 0x40,
}

var rawPPS = []byte{0x68, 0xce, 0x38, 0x80}

// SPS returns the Annex B SPS NAL unit.
func SPS() []byte { return append(append([]byte(nil), annexBPrefix...), rawSPS...) }

// PPS returns the Annex B PPS NAL unit.
func PPS() []byte { return append(append([]byte(nil), annexBPrefix...), rawPPS...) }

// emulationPrevention inserts 0x03 after every 00 00 and before a byte <= 3,
// per the RBSP-to-EBSP conversion rule.
func emulationPrevention(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp)+len(rbsp)/16)
	zeros := 0
	for _, x := range rbsp {
		if zeros >= 2 && x <= 3 {
			out = append(out, 0x03)
			zeros = 0
		}
		out = append(out, x)
		if x == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// BuildSlice returns an IDR (nal type 5) or non-IDR (type 1) slice NAL unit
// with deterministic filler so equal seeds give byte-identical payloads — the
// basis of fingerprint deduplication.
func BuildSlice(idr bool, frameNum uint, seed byte, payloadSize int) []byte {
	// Slice header kept trivially short; filler carries the distinguishing
	// bytes and is protected against start-code emulation.
	header := []byte{byte(frameNum & 0x0F)}
	rbsp := append(append([]byte(nil), header...), deterministicFill(seed, payloadSize)...)
	nalType := byte(1)
	refIDC := byte(0x20)
	if idr {
		nalType = 5
		refIDC = 0x60
	}
	nal := []byte{refIDC | nalType}
	return append(append(append([]byte(nil), annexBPrefix...), nal...),
		emulationPrevention(rbsp)...)
}

// deterministicFill produces n bytes from an LCG, so identical seeds yield
// byte-identical NAL payloads.
func deterministicFill(seed byte, n int) []byte {
	out := make([]byte, n)
	state := uint32(seed)*2654435761 + 1
	for i := range out {
		state = state*1103515245 + 12345
		out[i] = byte(state >> 16)
	}
	return out
}
