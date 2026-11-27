// Package manifest parses local HLS m3u8 and DASH MPD playlists into one
// canonical, nanosecond-based timeline. Parsers reject path escapes,
// duplicate segment numbers and invalid durations instead of guessing.
package manifest

import (
	"fmt"
	"path/filepath"
	"strings"

	"archive/internal/model"
)

// Format identifies the playlist standard a timeline was built from.
type Format string

const (
	FormatHLS  Format = "hls"
	FormatDASH Format = "dash"
)

// Segment is one canonical playlist segment on the shared timeline.
type Segment struct {
	Seq            int
	URI            string
	Duration       model.Nano
	Start          model.Nano
	End            model.Nano
	Discontinuity  bool
	Repaired       bool
	Gap            bool // true when the segment contains an explicit gap
	MissingPackets int
}

// Timeline is the normalised output of either parser. Segments are stored
// in strictly increasing sequence order with non-decreasing starts.
type Timeline struct {
	Format Format
	Base   string // playlist directory, used to constrain local segment paths
	Segments []Segment
}

// End returns the timeline end in nanoseconds (zero for an empty playlist).
func (t *Timeline) End() model.Nano {
	if len(t.Segments) == 0 {
		return 0
	}
	return t.Segments[len(t.Segments)-1].End
}

// SegmentAt finds the segment covering the given timestamp, or nil.
func (t *Timeline) SegmentAt(ts model.Nano) *Segment {
	for i := range t.Segments {
		s := &t.Segments[i]
		if ts >= s.Start && ts < s.End {
			return s
		}
	}
	return nil
}

// resolvePath confines a segment reference to the playlist directory.
// Absolute paths and any reference escaping the root (after cleaning) are
// rejected; remote URLs are refused because only local assets are allowed.
func resolvePath(base, ref string, log *model.FaultLog) (string, bool) {
	if ref == "" {
		log.Add(model.Fault{Phase: model.PhaseManifest, Kind: model.FaultPathRejected,
			Detail: "empty segment reference"})
		return "", false
	}
	if strings.Contains(ref, "://") {
		log.Add(model.Fault{Phase: model.PhaseManifest, Kind: model.FaultPathRejected,
			Detail: "remote reference is not allowed: " + ref})
		return "", false
	}
	cleanBase := filepath.Clean(base)
	cleanRef := filepath.Clean(ref)
	if filepath.IsAbs(cleanRef) {
		log.Add(model.Fault{Phase: model.PhaseManifest, Kind: model.FaultPathRejected,
			Detail: "absolute path is not allowed: " + ref})
		return "", false
	}
	joined := filepath.Join(cleanBase, cleanRef)
	rel, err := filepath.Rel(cleanBase, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		log.Add(model.Fault{Phase: model.PhaseManifest, Kind: model.FaultPathRejected,
			Detail: "reference escapes playlist root: " + ref})
		return "", false
	}
	return rel, true
}

// builder accumulates segments while enforcing the shared playlist rules.
type builder struct {
	tl     *Timeline
	log    *model.FaultLog
	seen   map[int]bool
	cursor model.Nano
}

func newBuilder(f Format, base string, log *model.FaultLog) *builder {
	return &builder{
		tl:    &Timeline{Format: f, Base: filepath.Clean(base)},
		log:   log,
		seen:  map[int]bool{},
	}
}

// add validates one segment and appends it to the canonical timeline.
func (b *builder) add(seq int, uri string, durSec float64, discont bool) error {
	if len(b.tl.Segments) >= model.MaxSegments {
		return model.InvariantError(model.PhaseManifest, "segment count exceeds %d", model.MaxSegments)
	}
	if b.seen[seq] {
		b.log.Add(model.Fault{Phase: model.PhaseManifest, Kind: model.FaultDuplicateSegment,
			Detail: fmt.Sprintf("duplicate segment number %d (%s)", seq, uri)})
		return fmt.Errorf("%w: duplicate segment %d", model.ErrInvariant, seq)
	}
	if durSec <= 0 || durSec > float64(model.MaxSegmentSeconds) {
		b.log.Add(model.Fault{Phase: model.PhaseManifest, Kind: model.FaultInvalidDuration,
			Detail: fmt.Sprintf("segment %d duration %.3fs out of (0,%ds]", seq, durSec, model.MaxSegmentSeconds)})
		return fmt.Errorf("%w: invalid duration %.3f for segment %d", model.ErrInvariant, durSec, seq)
	}
	path, ok := resolvePath(b.tl.Base, uri, b.log)
	if !ok {
		return fmt.Errorf("%w: rejected path %q", model.ErrInvariant, uri)
	}
	dur := model.Nano(durSec * float64(model.Second))
	seg := Segment{
		Seq: seq, URI: path, Duration: dur,
		Start: b.cursor, End: b.cursor + dur, Discontinuity: discont,
	}
	b.tl.Segments = append(b.tl.Segments, seg)
	b.seen[seq] = true
	b.cursor = seg.End
	return nil
}
