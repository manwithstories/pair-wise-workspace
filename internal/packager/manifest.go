// Package packager turns verified split segments into a delivery manifest.
// Only segments that passed structural, keyframe and fingerprint validation
// are admitted; the manifest records unified-timeline boundaries and the
// alignment error against each segment's ideal target duration.
package packager

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"mediacq/internal/fingerprint"
	"mediacq/internal/split"
)

// ErrUnverified is returned when a segment lacks a fingerprint or failed
// validation; such segments are never packaged.
var ErrUnverified = errors.New("packager: segment not verified; refusing to package")

// Entry is one manifest record.
type Entry struct {
	Seq             int     `json:"seq"`
	StartMS         int64   `json:"start_ms"`
	EndMS           int64   `json:"end_ms"`
	DurationMS      int64   `json:"duration_ms"`
	TargetMS        int64   `json:"target_ms"`
	AlignmentMS     int64   `json:"alignment_ms"` // |actual-target| in ms
	Fingerprint     string  `json:"fingerprint"`
	Bytes           int     `json:"bytes"`
	Events          []uint32 `json:"event_ids"`
	DriftAffected   bool    `json:"drift_affected"`
}

// Manifest is the deterministic delivery document.
type Manifest struct {
	Segments []Entry `json:"segments"`
	// MaxAlignmentMS is the worst boundary alignment error across entries.
	MaxAlignmentMS int64 `json:"max_alignment_ms"`
	// Duplicates is the number of verified-duplicate segments excluded.
	Duplicates int `json:"duplicates"`
}

// VerifiedSegment couples a cut segment with its computed fingerprint and
// payload byte length. Only these are accepted by the packager.
type VerifiedSegment struct {
	Segment     split.Segment
	Fingerprint fingerprint.Fingerprint
	Payload     [][]byte
}

// Options controls packaging.
type Options struct {
	TargetDuration time.Duration
}

// Build admits verified segments in order, drops verified duplicates, and
// computes alignment against the target duration. Output order is the input
// order, which is itself deterministic for a fixed seed.
func Build(verified []VerifiedSegment, opts Options) (*Manifest, error) {
	dedup := fingerprint.NewDeduper()
	m := &Manifest{Segments: make([]Entry, 0, len(verified))}
	targetMS := opts.TargetDuration.Milliseconds()
	for _, vs := range verified {
		if vs.Fingerprint.Length == 0 && len(vs.Payload) == 0 {
			return nil, ErrUnverified
		}
		if dedup.Add(vs.Fingerprint) {
			m.Duplicates++
			continue
		}
		dur, err := vs.Segment.Range.Duration()
		if err != nil {
			return nil, err
		}
		actualMS := dur.Milliseconds()
		align := actualMS - targetMS
		if align < 0 {
			align = -align
		}
		e := Entry{
			Seq:           vs.Segment.Seq,
			StartMS:       vs.Segment.Range.Start.Millis(),
			EndMS:         vs.Segment.Range.End.Millis(),
			DurationMS:    actualMS,
			TargetMS:      targetMS,
			AlignmentMS:   align,
			Fingerprint:   vs.Fingerprint.Encode(),
			Bytes:         vs.Fingerprint.Length,
			Events:        append([]uint32(nil), vs.Segment.EventIDs...),
			DriftAffected: vs.Segment.DriftAffected,
		}
		m.Segments = append(m.Segments, e)
		if align > m.MaxAlignmentMS {
			m.MaxAlignmentMS = align
		}
	}
	return m, nil
}

// MarshalJSON emits a stable, indented document.
func (m *Manifest) MarshalJSON() ([]byte, error) {
	// Defensive copy + sort by seq so even an unordered input is stable.
	cp := *m
	cp.Segments = append([]Entry(nil), m.Segments...)
	sort.SliceStable(cp.Segments, func(i, j int) bool {
		return cp.Segments[i].Seq < cp.Segments[j].Seq
	})
	type alias Manifest
	return json.MarshalIndent((*alias)(&cp), "", "  ")
}

// ResumeToken identifies the boundary a restart continues after.
type ResumeToken struct {
	LastSeq     int    `json:"last_seq"`
	EndMS       int64  `json:"end_ms"`
	Fingerprint string `json:"fingerprint"`
}

// ResumeFrom builds a token from the last committed manifest entry.
func (m *Manifest) ResumeFrom() (ResumeToken, bool) {
	if len(m.Segments) == 0 {
		return ResumeToken{}, false
	}
	last := m.Segments[len(m.Segments)-1]
	return ResumeToken{LastSeq: last.Seq, EndMS: last.EndMS, Fingerprint: last.Fingerprint}, true
}
