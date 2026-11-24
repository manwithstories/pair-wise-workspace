package fixture

import (
	"encoding/binary"
	"fmt"
)

// CorruptMP4Length rewrites the declared 32-bit size of a top-level box so it
// no longer matches its real extent. The demuxer must surface this as
// KindCorruptMedia while preserving the box's byte offset as first position.
//
// boxIndex selects among top-level boxes (0-based). growth is added to the
// declared size; a value larger than the remaining file makes the box
// overrun EOF, and a negative value shrinks it (both are corruptions).
func CorruptMP4Length(buf []byte, boxIndex int, growth int32) ([]byte, error) {
	out := append([]byte(nil), buf...)
	type loc struct{ off int }
	var boxes []loc
	pos := 0
	for pos+8 <= len(out) {
		size := int(binary.BigEndian.Uint32(out[pos:]))
		if size < 8 {
			return nil, fmt.Errorf("fixture: cannot parse box header at %d (size=%d)", pos, size)
		}
		boxes = append(boxes, loc{off: pos})
		pos += size
	}
	if boxIndex < 0 || boxIndex >= len(boxes) {
		return nil, fmt.Errorf("fixture: box index %d out of range (have %d)", boxIndex, len(boxes))
	}
	off := boxes[boxIndex].off
	orig := int32(binary.BigEndian.Uint32(out[off:]))
	binary.BigEndian.PutUint32(out[off:], uint32(orig+growth))
	return out, nil
}

// TruncateRTPCapture cuts the whole capture mid-packet, simulating a torn
// write. The target packet's length prefix stays intact while its body no
// longer reaches the declared length; the demuxer reports KindCorruptMedia
// at that packet index.
func TruncateRTPCapture(buf []byte, packetIndex int) ([]byte, error) {
	pos := 0
	for i := 0; ; i++ {
		if pos+4 > len(buf) {
			return nil, fmt.Errorf("fixture: packet %d not found", packetIndex)
		}
		n := int(binary.BigEndian.Uint32(buf[pos:]))
		end := pos + 4 + n
		if end > len(buf) {
			return nil, fmt.Errorf("fixture: RTP length overruns capture")
		}
		if i == packetIndex {
			// Keep the length prefix but drop the second half of the body.
			half := pos + 4 + n/2
			return append([]byte(nil), buf[:half]...), nil
		}
		pos = end
	}
}
