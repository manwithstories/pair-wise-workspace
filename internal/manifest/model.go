// Package manifest parses and regenerates DASH MPDs and HLS playlists.
//
// Both formats are projected onto one Manifest model so downstream modules
// (fragment indexer, transport, sync) never branch on container format.
// Segment boundaries are expressed in the shared 90 kHz media clock.
package manifest

import (
	"fmt"

	"avlab/internal/media"
)

// Kind identifies the manifest container format.
type Kind string

const (
	KindDASH Kind = "dash"
	KindHLS  Kind = "hls"
)

// Segment is one timeline entry. Discontinuity is set on the first segment
// of a new epoch (HLS #EXT-X-DISCONTINUITY).
type Segment struct {
	Index         uint32
	URL           string
	StartTick     media.MediaTime
	DurationTick  media.MediaTime
	Epoch         media.Epoch
	Discontinuity bool
	Keyframe      bool
}

// Representation is one bitrate level. All representations inside a manifest
// share aligned segment boundaries (segmentAlignment for DASH).
type Representation struct {
	ID        string
	Bandwidth uint32
	Width     uint32
	Height    uint32
	MIME      string
	Segments  []Segment
}

// Manifest is the normalized, format-independent playlist representation.
type Manifest struct {
	Kind            Kind
	TargetTick      media.MediaTime
	MinBufferTick   media.MediaTime
	Representations []Representation
}

// SegmentCount returns the number of segments per representation (they are
// aligned, so every representation has the same count).
func (m *Manifest) SegmentCount() int {
	if len(m.Representations) == 0 {
		return 0
	}
	return len(m.Representations[0].Segments)
}

// Validate enforces structural limits and timeline invariants. The returned
// error is always a *media.Fault.
func (m *Manifest) Validate() error {
	if m == nil {
		return media.NewFault(media.CodeInvalidArgument, false, "nil manifest")
	}
	if len(m.Representations) == 0 {
		return media.NewFault(media.CodeManifestDamaged, false, "no representations")
	}
	if len(m.Representations) > media.MaxRepresentations {
		return media.NewFault(media.CodeLimitExceeded, false,
			fmt.Sprintf("representations %d exceed hard limit %d",
				len(m.Representations), media.MaxRepresentations))
	}
	want := -1
	seen := map[string]bool{}
	for ri := range m.Representations {
		r := &m.Representations[ri]
		if r.ID == "" || seen[r.ID] {
			return media.NewFault(media.CodeManifestDamaged, false,
				"missing or duplicate representation id: "+r.ID)
		}
		seen[r.ID] = true
		if len(r.Segments) == 0 {
			return media.NewFault(media.CodeManifestDamaged, false,
				"representation "+r.ID+" has no segments")
		}
		if len(r.Segments) > media.MaxSegments {
			return media.NewFault(media.CodeLimitExceeded, false,
				"segment count exceeds hard limit")
		}
		if want == -1 {
			want = len(r.Segments)
		} else if len(r.Segments) != want {
			return media.NewFault(media.CodeManifestDamaged, false,
				"segment counts are not aligned across representations")
		}
		var start media.MediaTime
		for si := range r.Segments {
			s := &r.Segments[si]
			if s.DurationTick <= 0 {
				return media.NewFault(media.CodeManifestDamaged, false,
					fmt.Sprintf("rep %s segment %d has non-positive duration", r.ID, s.Index))
			}
			if s.StartTick != start {
				return media.NewFault(media.CodeManifestDamaged, false,
					fmt.Sprintf("rep %s segment %d gap in timeline", r.ID, s.Index))
			}
			next, err := start.Add(s.DurationTick)
			if err != nil {
				return media.WrapFault(media.CodeTimestampOverflow, false,
					"segment timeline overflow", err)
			}
			start = next
		}
	}
	return nil
}

// checkSize rejects manifests above the 1 MiB hard limit.
func checkSize(n int) error {
	if n > media.MaxManifestBytes {
		return media.NewFault(media.CodeManifestTooLarge, false,
			fmt.Sprintf("manifest %d bytes exceeds limit %d", n, media.MaxManifestBytes))
	}
	return nil
}
