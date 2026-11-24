// Package manifest parses scoped DASH SegmentTemplate and HLS master/media
// playlists and projects both onto one rational millisecond clock.
package manifest

import (
	"mediac/internal/core"
)

// Discontinuity marks a clock reset boundary inside a representation. The
// segment at Index starts a fresh media timeline; durations remain valid but
// raw timestamps must not be compared across the boundary.
type Discontinuity struct {
	Index   int   `json:"index"`    // sequence number of the first segment after reset
	Line    int   `json:"line"`     // playlist line declaring the boundary
	StartMs int64 `json:"start_ms"` // projected position on the unified clock
}

// Segment is one media chunk on the unified millisecond timeline.
type Segment struct {
	Index      int    `json:"index"`              // manifest sequence number
	URL        string `json:"url"`                // reference as written in the manifest
	AbsPath    string `json:"abs_path,omitempty"` // resolved, root-contained path
	StartMs    int64  `json:"start_ms"`
	DurationMs int64  `json:"duration_ms"`
	Line       int    `json:"line"` // 1-based manifest line
}

// EndMs returns the projected exclusive end of the segment.
func (s Segment) EndMs() int64 { return s.StartMs + s.DurationMs }

// Representation is one encoded rendition (DASH AdaptationSet/Representation
// pair or an HLS variant/subtitle media entry).
type Representation struct {
	ID              string          `json:"id"`
	Codec           string          `json:"codec"`
	Width           int             `json:"width,omitempty"`
	Height          int             `json:"height,omitempty"`
	TargetBPS       int64           `json:"target_bps"`
	Timescale       int64           `json:"timescale"`
	InitURL         string          `json:"init_url,omitempty"`
	InitPath        string          `json:"init_path,omitempty"`
	Segments        []Segment       `json:"segments"`
	Discontinuities []Discontinuity `json:"discontinuities,omitempty"`
}

// Kind identifies the manifest dialect.
type Kind string

const (
	KindDASH Kind = "dash"
	KindHLS  Kind = "hls"
)

// Manifest is the normalized output consumed by transport recovery and the
// bitstream analyzer.
type Manifest struct {
	Kind            Kind
	Root            string           // input root used for path resolution
	SourcePath      string           // manifest file location
	Representations []Representation `json:"representations"`
	Faults          []core.Fault     `json:"faults,omitempty"`
}

// SegmentCount sums segments over all representations.
func (m *Manifest) SegmentCount() int {
	n := 0
	for _, r := range m.Representations {
		n += len(r.Segments)
	}
	return n
}

// FindSegment locates a segment by representation id and sequence index.
func (m *Manifest) FindSegment(repID string, index int) (*Representation, *Segment) {
	for i := range m.Representations {
		r := &m.Representations[i]
		if r.ID != repID {
			continue
		}
		for j := range r.Segments {
			if r.Segments[j].Index == index {
				return r, &r.Segments[j]
			}
		}
	}
	return nil, nil
}

// IsAfterDiscontinuity reports whether the segment starts a fresh clock.
func (r *Representation) IsAfterDiscontinuity(index int) bool {
	for _, d := range r.Discontinuities {
		if d.Index == index {
			return true
		}
	}
	return false
}
