// Package hash produces reproducible per-segment digests and the manifest
// that lists them.  A digest covers the segment's ordinal, its exact start
// and end ticks, and every frame payload in order, all length-prefixed so
// that concatenation is unambiguous.  Hashing is pure: the same inputs give
// the same digest on every run and on every machine.
package hash

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
	"mediapack/internal/index"
)

// Limits bounds manifest size.
type Limits struct{ MaxSegments int }

// DefaultLimits matches the segment planner.
func DefaultLimits() Limits { return Limits{MaxSegments: 36000} }

// Frame is one payload contributing to a segment.
type Frame struct {
	Track   string
	Sample  int
	SHA256  string
	Payload []byte
}

// Segment describes one unit to hash.
type Segment struct {
	Ordinal    int
	Start, End clock.Ticks
	Frames     []Frame
}

// Entry is the durable, reviewable record of one hashed segment.
type Entry struct {
	Ordinal         int      `json:"ordinal"`
	Start           string   `json:"start"`
	End             string   `json:"end"`
	Frames          int      `json:"frames"`
	SHA256          string   `json:"sha256"`
	Blocked         bool     `json:"blocked"`
	BlockedBy       string   `json:"blocked_by,omitempty"`
	Representations []string `json:"representations"`
}

// Manifest is the reviewable output of a run.
type Manifest struct {
	SegmentTicks string  `json:"segment_ticks"`
	Segments     []Entry `json:"segments"`
	// Blocked marks the whole manifest as incomplete; no blocked segment is
	// ever listed as successful.
	Blocked bool `json:"blocked"`
}

// Digest computes the digest of one segment.  It returns an attributable
// fault when a frame is missing or a payload does not match its recorded
// checksum; such a segment must not enter the success manifest.
func Digest(seg Segment, reps []string) (Entry, error) {
	if seg.Ordinal < 1 {
		return Entry{}, fault.New(fault.ClassInternal, "hash", "*", "invalid-ordinal",
			"segment ordinal must be positive")
	}
	if seg.End.Cmp(seg.Start) <= 0 {
		return Entry{}, fault.New(fault.ClassInput, "hash", "*", "empty-segment",
			fmt.Sprintf("segment %d has non-positive duration", seg.Ordinal)).WithSegment(seg.Ordinal)
	}
	h := sha256.New()
	writeTicks(h, seg.Start)
	writeTicks(h, seg.End)
	writeString(h, strings.Join(reps, ","))
	for i, f := range seg.Frames {
		if f.Payload == nil {
			return Entry{}, fault.New(fault.ClassInput, "hash", f.Track, "missing-payload",
				fmt.Sprintf("segment %d frame %d has no payload", seg.Ordinal, f.Sample)).
				WithSegment(seg.Ordinal).WithFrame(f.Sample)
		}
		if f.SHA256 != "" && checksum(f.Payload) != f.SHA256 {
			return Entry{}, fault.New(fault.ClassInput, "hash", f.Track, "checksum-mismatch",
				fmt.Sprintf("segment %d frame %d payload changed after validation", seg.Ordinal, f.Sample)).
				WithSegment(seg.Ordinal).WithFrame(f.Sample)
		}
		writeString(h, f.Track)
		writeUint(h, uint64(f.Sample))
		writeUint(h, uint64(i))
		writeBytes(h, f.Payload)
	}
	return Entry{
		Ordinal: seg.Ordinal, Start: seg.Start.String(), End: seg.End.String(),
		Frames: len(seg.Frames), SHA256: hex.EncodeToString(h.Sum(nil)),
		Representations: append([]string(nil), reps...),
	}, nil
}

// Build hashes every segment.  A failure in one segment is recorded as
// blocked with the first failing dependency and does not corrupt the hashes
// of unaffected segments; the returned manifest is marked incomplete.
func Build(segs []Segment, reps []string, segmentTicks clock.Ticks, limits Limits) (*Manifest, error) {
	if len(segs) > limits.MaxSegments {
		return nil, fault.New(fault.ClassLimit, "hash", "*", "too-many-segments",
			fmt.Sprintf("%d segments exceeds limit %d", len(segs), limits.MaxSegments))
	}
	ordered := append([]Segment(nil), segs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Ordinal < ordered[j].Ordinal })

	m := &Manifest{SegmentTicks: segmentTicks.String()}
	seen := make(map[int]bool, len(ordered))
	var first *fault.Error
	for _, seg := range ordered {
		if seen[seg.Ordinal] {
			return nil, fault.New(fault.ClassInput, "hash", "*", "duplicate-segment",
				fmt.Sprintf("segment ordinal %d appears twice", seg.Ordinal))
		}
		seen[seg.Ordinal] = true
		entry, err := Digest(seg, reps)
		if err != nil {
			if fe, ok := fault.As(err); ok {
				entry = Entry{Ordinal: seg.Ordinal, Start: seg.Start.String(), End: seg.End.String(),
					Frames: len(seg.Frames), Blocked: true, BlockedBy: fe.Track,
					Representations: append([]string(nil), reps...)}
				if first == nil {
					first = fe
				}
			} else {
				return nil, err
			}
			m.Blocked = true
		}
		m.Segments = append(m.Segments, entry)
	}
	if first != nil {
		return m, first
	}
	return m, nil
}

// AllMatch reports whether two manifests list the same digests in order.
// It is the check that proves a resumed run reproduced the original output.
func AllMatch(a, b *Manifest) bool {
	if a == nil || b == nil || len(a.Segments) != len(b.Segments) {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i].SHA256 != b.Segments[i].SHA256 ||
			a.Segments[i].Ordinal != b.Segments[i].Ordinal {
			return false
		}
	}
	return true
}

// FromIndex builds one segment's frames from an index table and payloads.
// Samples are selected by start tick in [start, end).
func FromIndex(rep string, table *index.Table, start, end clock.Ticks,
	payloads map[int][]byte) ([]Frame, error) {
	var frames []Frame
	for i := range table.Starts {
		s := table.Starts[i]
		if s.Cmp(start) < 0 || s.Cmp(end) >= 0 {
			continue
		}
		frames = append(frames, Frame{Track: rep, Sample: i + 1, Payload: payloads[i+1]})
	}
	if len(frames) == 0 {
		return nil, fault.New(fault.ClassInput, "hash", rep, "empty-segment",
			fmt.Sprintf("no frames in [%s,%s)", start, end))
	}
	return frames, nil
}

func checksum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func writeTicks(h interface{ Write([]byte) (int, error) }, t clock.Ticks) {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(t.Num()))
	binary.BigEndian.PutUint64(buf[8:], uint64(t.Den()))
	h.Write(buf[:])
}

func writeUint(h interface{ Write([]byte) (int, error) }, v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	h.Write(buf[:])
}

func writeString(h interface{ Write([]byte) (int, error) }, s string) {
	writeBytes(h, []byte(s))
}

func writeBytes(h interface{ Write([]byte) (int, error) }, b []byte) {
	writeUint(h, uint64(len(b)))
	h.Write(b)
}
