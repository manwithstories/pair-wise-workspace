package source

import (
	"fmt"

	"mediapipe/internal/media"
)

// ValidateTimestamps enforces non-regressing PTS and DTS on every stream. It
// deliberately returns a located error rather than clamping/rewriting values.
// Input is expected in demux/decode order for DTS and presentation order for
// PTS; callers handling B-frame reordering should run PTS validation after
// presentation sorting or use ValidateDTS separately.
func ValidateTimestamps(packets []media.Packet) error {
	lastPTS := map[int]int64{}
	lastDTS := map[int]int64{}
	havePTS := map[int]bool{}
	haveDTS := map[int]bool{}
	for i, p := range packets {
		pos := p.FileOffset
		if pos <= 0 {
			pos = int64(i)
		}
		if haveDTS[p.StreamIndex] && p.DTS < lastDTS[p.StreamIndex] {
			return media.TimestampError(p.StreamIndex, pos,
				fmt.Sprintf("DTS moved backwards from %d to %d", lastDTS[p.StreamIndex], p.DTS))
		}
		lastDTS[p.StreamIndex] = p.DTS
		haveDTS[p.StreamIndex] = true

		if havePTS[p.StreamIndex] && p.PTS < lastPTS[p.StreamIndex] {
			return media.TimestampError(p.StreamIndex, pos,
				fmt.Sprintf("PTS moved backwards from %d to %d", lastPTS[p.StreamIndex], p.PTS))
		}
		lastPTS[p.StreamIndex] = p.PTS
		havePTS[p.StreamIndex] = true
	}
	return nil
}

// ValidateDTS only checks decode order and is safe directly on astiav packet
// order even when PTS is reordered by B-frames.
func ValidateDTS(packets []media.Packet) error {
	last := map[int]int64{}
	have := map[int]bool{}
	for i, p := range packets {
		pos := p.FileOffset
		if pos <= 0 {
			pos = int64(i)
		}
		if have[p.StreamIndex] && p.DTS < last[p.StreamIndex] {
			return media.TimestampError(p.StreamIndex, pos,
				fmt.Sprintf("DTS moved backwards from %d to %d", last[p.StreamIndex], p.DTS))
		}
		last[p.StreamIndex] = p.DTS
		have[p.StreamIndex] = true
	}
	return nil
}

// SortByPresentation returns a PTS-ordered copy for cut/fingerprint consumers.
func SortByPresentation(packets []media.Packet) []media.Packet {
	out := append([]media.Packet(nil), packets...)
	// Small insertion sort keeps equal-timestamp stream ordering stable enough
	// for deterministic fixtures.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && presentationLess(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func presentationLess(a, b media.Packet) bool {
	ta, tb := a.PTSSec(), b.PTSSec()
	if ta != tb {
		return ta < tb
	}
	if a.StreamIndex != b.StreamIndex {
		return a.StreamIndex < b.StreamIndex
	}
	return a.Sequence < b.Sequence
}
