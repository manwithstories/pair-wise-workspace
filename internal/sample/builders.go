// Package sample generates deterministic, dependency-free synthetic media.
package sample

import (
	"fmt"

	"mediad/internal/codec"
)

func fill(dst []byte, seed int64, tag byte) {
	x := uint64(seed)*1_000_003 + uint64(tag)<<32
	for i := range dst {
		// SplitMix-style diffusion makes neighboring AU fixtures distinct;
		// setting the high bit prevents accidental Annex B start codes.
		x = x*6364136223846793005 + 1442695040888963407 + uint64(i)*1_000_003
		dst[i] = byte(x>>33) | 0x80
	}
}

func padAnnexB(base []byte, total int, seed int64, tag byte) []byte {
	if total < len(base) {
		panic(fmt.Sprintf("requested AU size %d < base %d", total, len(base)))
	}
	out := append([]byte(nil), base...)
	extra := make([]byte, total-len(base))
	fill(extra, seed, tag)
	return append(out, extra...)
}

// H264AU builds a small valid-boundary Annex B AU padded to total bytes.
func H264AU(key bool, total int, seed int64, auIndex int) []byte {
	var base []byte
	base = append(base, 0, 0, 0, 1, 0x67, 0x42, 0x00, 0x1e) // SPS-ish
	base = append(base, 0, 0, 0, 1, 0x68, 0xce, 0x38, 0x80) // PPS-ish
	if key {
		base = append(base, 0, 0, 0, 1, 0x65, 0x88, 0x80) // IDR
	} else {
		base = append(base, 0, 0, 0, 1, 0x41, 0x9a, 0x00) // non-IDR
	}
	return padAnnexB(base, total, seed+int64(auIndex), byte(auIndex))
}

// H265AU builds a small valid-boundary Annex B AU padded to total bytes.
func H265AU(key bool, total int, seed int64, auIndex int) []byte {
	var base []byte
	base = append(base, 0, 0, 0, 1, 0x40, 0x01, 0x0c, 0x01) // VPS
	base = append(base, 0, 0, 0, 1, 0x42, 0x01, 0x01, 0x60) // SPS
	base = append(base, 0, 0, 0, 1, 0x44, 0x01, 0xc0, 0x70) // PPS
	if key {
		base = append(base, 0, 0, 0, 1, 0x26, 0x01, 0xaf, 0x08) // IDR_W_RADL
	} else {
		base = append(base, 0, 0, 0, 1, 0x02, 0x01, 0xaa, 0x00) // TRAIL_R
	}
	return padAnnexB(base, total, seed+int64(auIndex), byte(auIndex+7))
}

func av1OBU(typ int, payload []byte) []byte {
	h := byte(typ<<3) | 0x02
	out := []byte{h}
	out = append(out, codec.EncodeLEB128(uint64(len(payload)))...)
	return append(out, payload...)
}

// AV1AU builds length-delimited OBU fixtures padded to total bytes.
func AV1AU(key bool, total int, seed int64, auIndex int) []byte {
	seqPayload := []byte{0x01, 0x0c, 0x3a, 0x00, 0x00, 0x03, 0x80}
	seq := av1OBU(codec.OBUSequenceHeader, seqPayload)
	// Solve payload length against the variable-width LEB128 field.
	payloadLen := total - len(seq) - 1 - 3
	if payloadLen < 2 {
		payloadLen = 2
	}
	for attempt := 0; attempt < 3; attempt++ {
		lebLen := len(codec.EncodeLEB128(uint64(payloadLen)))
		next := total - len(seq) - 1 - lebLen
		if next == payloadLen || next < 1 {
			break
		}
		payloadLen = next
	}
	payload := make([]byte, payloadLen)
	if key {
		payload[0] = 0x10 // key frame, show_frame
	} else {
		payload[0] = 0x30 // inter frame, show_frame
	}
	fill(payload[1:], seed+int64(auIndex), byte(auIndex+3))
	frame := av1OBU(codec.OBUFrame, payload)
	out := append(append([]byte(nil), seq...), frame...)
	if len(out) != total {
		// Tiny callers may not leave room for the exact padded layout; use a
		// valid small OBU rather than emitting a mismatched length field.
		return AV1AUSmall(key, total)
	}
	return out
}

// AV1AUSmall is used only for very small unit-test-sized requests.
func AV1AUSmall(key bool, total int) []byte {
	frameByte := byte(0x30)
	if key {
		frameByte = 0x10
	}
	payload := []byte{frameByte, 0xaa}
	frame := av1OBU(codec.OBUFrame, payload)
	seq := av1OBU(codec.OBUSequenceHeader, []byte{1, 2, 3})
	out := append(seq, frame...)
	if total > len(out) {
		// Add a metadata OBU rather than violating OBU length framing.
		meta := av1OBU(5, make([]byte, total-len(out)-2))
		out = append(out, meta...)
	}
	return out
}
