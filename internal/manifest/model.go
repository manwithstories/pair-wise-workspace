package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	DefaultTemplate = "seg/$RepresentationID$/$Number%05d$.m4s"
	CodecVersion    = 1
)

// Error is a stable, machine-readable validation error. Code never contains
// user data and is suitable for tests and CLI exit-code decisions.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Message + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

type Segment struct {
	Number        uint64
	StartUS       int64
	DurationUS    int64
	RawStartUS    int64
	DriftUS       int64
	Truncated     bool
	Discontinuity bool
}

func (s Segment) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%d\n%d\n%t\n", s.Number, s.StartUS, s.DurationUS, s.Truncated)
	return hex.EncodeToString(h.Sum(nil))
}

type Representation struct {
	ID            string
	Bandwidth     uint64
	AudioOffsetUS int64
	Segments      []Segment
}

func (r Representation) TotalDurationUS() int64 {
	var total int64
	for _, s := range r.Segments {
		total += s.DurationUS
	}
	return total
}

type Discontinuity struct {
	RepresentationID string
	SegmentNumber    uint64
	RawStartUS       int64
	GridStartUS      int64
	DriftUS          int64
	Direction        int
}

type ResyncPoint struct {
	RepresentationID string
	SegmentNumber    uint64
	GridStartUS      int64
	DriftUS          int64
	Direction        int
}

type RepresentationStats struct {
	RepresentationID string
	Bandwidth        uint64
	Samples          int
	TotalAbsUS       int64
	MaxDriftUS       int64
	P95DriftUS       int64
	ResyncPoints     int
}

type Window struct {
	Version     uint64
	StartNumber uint64
	Size        int
}

type Manifest struct {
	TargetUS        int64
	ToleranceUS     int64
	Template        string
	Representations []Representation
	Discontinuities []Discontinuity
	ResyncPoints    []ResyncPoint
	Stats           []RepresentationStats
	Window          Window
}

func (m *Manifest) Validate() error {
	if m.TargetUS <= 0 {
		return NewError("manifest.invalid_target", "target duration must be positive")
	}
	if m.ToleranceUS <= 0 || m.ToleranceUS > m.TargetUS/2 {
		return NewError("manifest.invalid_tolerance", "tolerance must be in (0, target/2]")
	}
	if strings.TrimSpace(m.Template) == "" {
		m.Template = DefaultTemplate
	}
	if _, err := ParseTemplate(m.Template); err != nil {
		return err
	}
	if len(m.Representations) == 0 {
		return NewError("manifest.no_representations", "at least one representation is required")
	}
	ids := make(map[string]struct{}, len(m.Representations))
	for i := range m.Representations {
		r := &m.Representations[i]
		if strings.TrimSpace(r.ID) == "" {
			return NewError("manifest.empty_representation_id", "representation %d has an empty ID", i)
		}
		if r.Bandwidth == 0 {
			return NewError("manifest.zero_bandwidth", "representation %q has zero bandwidth", r.ID)
		}
		if _, ok := ids[r.ID]; ok {
			return NewError("manifest.duplicate_representation", "duplicate representation ID %q", r.ID)
		}
		ids[r.ID] = struct{}{}
		if len(r.Segments) == 0 {
			return NewError("manifest.empty_representation", "representation %q has no segments", r.ID)
		}
		previous := uint64(0)
		for j, s := range r.Segments {
			if s.Number == 0 {
				return NewError("manifest.zero_segment_number", "representation %q has segment number zero", r.ID)
			}
			if j > 0 && s.Number <= previous {
				return NewError("manifest.segment_order", "representation %q segment numbers are not strictly increasing", r.ID)
			}
			if s.DurationUS <= 0 {
				return NewError("manifest.invalid_duration", "representation %q segment %d has non-positive duration", r.ID, s.Number)
			}
			previous = s.Number
		}
	}
	SortRepresentations(m.Representations)
	deduped := m.Representations[:0]
	var previousBandwidth uint64
	for i, rep := range m.Representations {
		if i > 0 && rep.Bandwidth == previousBandwidth {
			continue
		}
		deduped = append(deduped, rep)
		previousBandwidth = rep.Bandwidth
	}
	m.Representations = deduped
	sort.Slice(m.Discontinuities, func(i, j int) bool {
		if m.Discontinuities[i].RepresentationID != m.Discontinuities[j].RepresentationID {
			return m.Discontinuities[i].RepresentationID < m.Discontinuities[j].RepresentationID
		}
		return m.Discontinuities[i].SegmentNumber < m.Discontinuities[j].SegmentNumber
	})
	sort.Slice(m.ResyncPoints, func(i, j int) bool {
		if m.ResyncPoints[i].RepresentationID != m.ResyncPoints[j].RepresentationID {
			return m.ResyncPoints[i].RepresentationID < m.ResyncPoints[j].RepresentationID
		}
		return m.ResyncPoints[i].SegmentNumber < m.ResyncPoints[j].SegmentNumber
	})
	sort.Slice(m.Stats, func(i, j int) bool { return m.Stats[i].RepresentationID < m.Stats[j].RepresentationID })
	if len(m.Discontinuities) == 0 {
		m.Discontinuities = nil
	}
	if len(m.ResyncPoints) == 0 {
		m.ResyncPoints = nil
	}
	if len(m.Stats) == 0 {
		m.Stats = nil
	}
	return nil
}

func SortRepresentations(reps []Representation) {
	sort.SliceStable(reps, func(i, j int) bool {
		if reps[i].Bandwidth != reps[j].Bandwidth {
			return reps[i].Bandwidth < reps[j].Bandwidth
		}
		return reps[i].ID < reps[j].ID
	})
	for i := range reps {
		sort.SliceStable(reps[i].Segments, func(a, b int) bool {
			return reps[i].Segments[a].Number < reps[i].Segments[b].Number
		})
	}
}

func FindRepresentation(m *Manifest, id string) (*Representation, bool) {
	for i := range m.Representations {
		if m.Representations[i].ID == id {
			return &m.Representations[i], true
		}
	}
	return nil, false
}

// RollWindow keeps the most recent n segments of every representation.
// Numbers are never renumbered. Version is incremented exactly once.
func RollWindow(m *Manifest, n int) error {
	if n <= 0 {
		return NewError("manifest.invalid_window", "window size must be positive")
	}
	start := uint64(0)
	for i := range m.Representations {
		segs := m.Representations[i].Segments
		if len(segs) > n {
			segs = segs[len(segs)-n:]
		}
		m.Representations[i].Segments = segs
		if len(segs) > 0 && (start == 0 || segs[0].Number < start) {
			start = segs[0].Number
		}
	}
	m.Window.Version++
	m.Window.StartNumber = start
	m.Window.Size = n
	retained := make(map[string]uint64, len(m.Representations))
	for _, rep := range m.Representations {
		if len(rep.Segments) > 0 {
			retained[rep.ID] = rep.Segments[0].Number
		}
	}
	discontinuities := m.Discontinuities[:0]
	for _, point := range m.Discontinuities {
		if first, ok := retained[point.RepresentationID]; ok && point.SegmentNumber >= first {
			discontinuities = append(discontinuities, point)
		}
	}
	m.Discontinuities = discontinuities
	resyncs := m.ResyncPoints[:0]
	for _, point := range m.ResyncPoints {
		if first, ok := retained[point.RepresentationID]; ok && point.SegmentNumber >= first {
			resyncs = append(resyncs, point)
		}
	}
	m.ResyncPoints = resyncs
	m.Stats = nil
	return m.Validate()
}

// MergeRepresentations merges a newer period into an existing representation.
// Overlapping segment numbers use the newer copy. Equal versions may not
// disagree about an overlapping segment.
func MergeRepresentations(old, newer Representation, oldVersion, newVersion uint64) (Representation, error) {
	if old.ID != newer.ID || old.Bandwidth != newer.Bandwidth {
		return Representation{}, NewError("merge.representation_mismatch", "representation metadata conflicts for %q", old.ID)
	}
	if newVersion <= oldVersion && old.AudioOffsetUS != newer.AudioOffsetUS {
		return Representation{}, NewError("merge.content_conflict", "conflicting audio offset for %q at version %d", newer.ID, newVersion)
	}
	merged := old
	merged.AudioOffsetUS = newer.AudioOffsetUS
	byNumber := make(map[uint64]Segment, len(old.Segments)+len(newer.Segments))
	for _, s := range old.Segments {
		byNumber[s.Number] = s
	}
	for _, s := range newer.Segments {
		if previous, ok := byNumber[s.Number]; ok {
			if newVersion <= oldVersion && previous.Fingerprint() != s.Fingerprint() {
				return Representation{}, NewError("merge.content_conflict", "conflicting segment %q/%d at version %d", newer.ID, s.Number, newVersion)
			}
		}
		byNumber[s.Number] = s
	}
	merged.Segments = make([]Segment, 0, len(byNumber))
	for _, s := range byNumber {
		merged.Segments = append(merged.Segments, s)
	}
	sort.Slice(merged.Segments, func(i, j int) bool { return merged.Segments[i].Number < merged.Segments[j].Number })
	return merged, nil
}
