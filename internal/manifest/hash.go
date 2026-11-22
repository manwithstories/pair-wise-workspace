package manifest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"mediaroute/internal/adapter"
)

// SegmentFile returns the canonical file name of a segment payload.
func SegmentFile(index int) string { return fmt.Sprintf("segment-%06d.seg", index) }

// Canonical renders a segment and its events deterministically: a fixed
// header followed by events sorted by (track, timestamp, seq), each field
// length-prefixed or fixed-width so the encoding is unambiguous.
func Canonical(seg Segment, events []adapter.Event) []byte {
	sorted := append([]adapter.Event(nil), events...)
	SortEvents(sorted)
	out := make([]byte, 0, 128+64*len(sorted))
	out = binary.BigEndian.AppendUint64(out, uint64(seg.Index))
	out = binary.BigEndian.AppendUint64(out, uint64(seg.StartNs))
	out = binary.BigEndian.AppendUint64(out, uint64(seg.EndNs))
	for _, ev := range sorted {
		out = appendField(out, []byte(ev.Track))
		out = appendField(out, []byte(ev.Kind))
		out = binary.BigEndian.AppendUint64(out, uint64(ev.Seq))
		out = binary.BigEndian.AppendUint64(out, uint64(ev.TimestampNs))
		out = binary.BigEndian.AppendUint64(out, uint64(ev.DurationNs))
		if ev.Keyframe {
			out = append(out, 1)
		} else {
			out = append(out, 0)
		}
		out = appendField(out, ev.Payload)
	}
	return out
}

func appendField(dst, field []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(field)))
	return append(dst, field...)
}

// HashSegment returns the SHA-256 hex digest of the canonical encoding.
func HashSegment(seg Segment, events []adapter.Event) string {
	sum := sha256.Sum256(Canonical(seg, events))
	return hex.EncodeToString(sum[:])
}

// WriteSegment writes the canonical segment payload into dir.
func WriteSegment(dir string, seg Segment, events []adapter.Event) error {
	data := Canonical(seg, events)
	return os.WriteFile(filepath.Join(dir, SegmentFile(seg.Index)), data, 0o644)
}

// HashFile recomputes the SHA-256 of a stored segment file.
func HashFile(dir string, index int) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, SegmentFile(index)))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
