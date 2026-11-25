package timeline

import (
	"fmt"
	"strings"

	"archivecheck/internal/demux"
	"archivecheck/internal/media"
)

// TrackMapping binds one validated edit to a concrete sample range on a source
// track and assigns the output track it will occupy in the archive fragment.
type TrackMapping struct {
	EditID      string
	Segment     string
	SourceTrack int
	OutputTrack int
	FirstSample int // inclusive, 0-based within the source track
	LastSample  int // exclusive
	TimeBase    media.Rational
}

// SampleCount is the number of source samples this mapping will remux.
func (m TrackMapping) SampleCount() int { return m.LastSample - m.FirstSample }

// Canonical renders a deterministic, field-separated representation used as a
// cache-key component. It never includes free-form text, so two identical
// mappings always byte-equal.
func (m TrackMapping) Canonical() string {
	return fmt.Sprintf("%s|%s|%d->%d|%d:%d|%d/%d",
		m.EditID, m.Segment, m.SourceTrack, m.OutputTrack,
		m.FirstSample, m.LastSample, m.TimeBase.Num, m.TimeBase.Den)
}

// MappedEdit pairs a resolved edit with the samples selected for remux.
type MappedEdit struct {
	Resolved ResolvedEdit
	Mapping  TrackMapping
	Samples  []demux.Sample
}

// BuildMapping matches every resolved edit against the demuxed tracks of its
// segment and selects the samples intersecting the edit's source interval.
// Selection stays in the source track's integer time base; no float is used.
func BuildMapping(re ResolvedEdit, tracks []demux.TrackSamples, outputTrack int) (MappedEdit, *media.Fault) {
	var chosen *demux.TrackSamples
	for i := range tracks {
		if tracks[i].Meta.Track == re.Edit.Track {
			chosen = &tracks[i]
			break
		}
	}
	if chosen == nil {
		return MappedEdit{}, media.NewFault(media.KindInvalidManifest, re.Edit.Segment,
			re.Edit.Track, -1, "edit %s references absent track %d", re.Edit.ID, re.Edit.Track).WithEdit(re.Edit.ID)
	}

	// Edit endpoints are in the source track's time base. Locate the sample
	// interval [in,out) by comparing integer DTS values.
	inTicks := re.Edit.In.Ticks
	outTicks := re.Edit.Out.Ticks
	first, last := -1, len(chosen.Samples)
	for i, s := range chosen.Samples {
		end := s.DTS + s.Duration
		if first < 0 && end > inTicks {
			first = i // first sample overlapping the in-point
		}
		if s.DTS >= outTicks {
			last = i // first sample at/after the out-point
			break
		}
	}
	if first < 0 {
		first = len(chosen.Samples)
	}
	if first > last {
		first = last
	}

	m := TrackMapping{
		EditID: re.Edit.ID, Segment: re.Edit.Segment,
		SourceTrack: chosen.Meta.Track, OutputTrack: outputTrack,
		FirstSample: first, LastSample: last, TimeBase: chosen.Meta.TimeBase,
	}
	return MappedEdit{Resolved: re, Mapping: m, Samples: chosen.Samples[first:last]}, nil
}

// MappingDigestInput concatenates canonical mappings plus the edit boundaries
// in master ticks, giving the cache layer a single stable string to hash.
func MappingDigestInput(masterTB media.Rational, mappings []TrackMapping, edits []ResolvedEdit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tb=%d/%d;", masterTB.Num, masterTB.Den)
	for _, e := range edits {
		fmt.Fprintf(&b, "edit=%s,%d:%d;", e.Edit.ID, e.InMaster, e.OutMaster)
	}
	for _, m := range mappings {
		b.WriteString(m.Canonical())
		b.WriteByte(';')
	}
	return b.String()
}

// Correlate joins a demux/index fault to the edit owning the same segment and
// track, returning the edit id (empty when no edit claims the location).
func Correlate(f *media.Fault, edits []ResolvedEdit) string {
	if f.EditID != "" {
		return f.EditID
	}
	for _, e := range edits {
		if e.Edit.Segment == f.Segment && (f.Track == 0 || e.Edit.Track == f.Track) {
			return e.Edit.ID
		}
	}
	return ""
}
