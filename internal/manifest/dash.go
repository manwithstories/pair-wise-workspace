package manifest

import (
	"encoding/xml"
	"fmt"
	"strings"

	"mediascan/internal/core"
)

// MPDSegment is one expanded SegmentTimeline entry, in timescale units.
type MPDSegment struct {
	T uint64 // presentation time (before presentationTimeOffset is removed)
	D uint64 // duration
}

// MPDRepresentation is the normalized form of one Representation.
type MPDRepresentation struct {
	ID          string
	Bandwidth   uint64
	Timescale   uint64
	PTO         uint64 // presentationTimeOffset
	StartNumber uint64
	Media       string // media URL template
	Segments    []MPDSegment
}

// MPD is the normalized, single-Period subset of a DASH manifest that
// this tool supports.
type MPD struct {
	Type                      string
	MediaPresentationDuration string
	PeriodID                  string
	PeriodDuration            string
	Representations           []MPDRepresentation
}

// xml-wire structs (attribute order fixed for deterministic encoding).

type xmlS struct {
	T *uint64 `xml:"t,attr,omitempty"`
	D uint64  `xml:"d,attr"`
	R *int    `xml:"r,attr,omitempty"`
}

type xmlSegmentTemplate struct {
	Timescale   uint64 `xml:"timescale,attr"`
	PTO         uint64 `xml:"presentationTimeOffset,attr"`
	StartNumber uint64 `xml:"startNumber,attr"`
	Media       string `xml:"media,attr"`
	S           []xmlS `xml:"SegmentTimeline>S"`
}

type xmlRepresentation struct {
	ID              string             `xml:"id,attr"`
	Bandwidth       uint64             `xml:"bandwidth,attr"`
	SegmentTemplate xmlSegmentTemplate `xml:"SegmentTemplate"`
}

type xmlAdaptationSet struct {
	ID              string              `xml:"id,attr"`
	Representations []xmlRepresentation `xml:"Representation"`
}

type xmlPeriod struct {
	ID             string             `xml:"id,attr"`
	Duration       string             `xml:"duration,attr"`
	AdaptationSets []xmlAdaptationSet `xml:"AdaptationSet"`
}

type xmlMPD struct {
	XMLName                   xml.Name   `xml:"MPD"`
	Type                      string     `xml:"type,attr"`
	MediaPresentationDuration string     `xml:"mediaPresentationDuration,attr"`
	Periods                   []xmlPeriod `xml:"Period"`
}

// ParseDASH parses an MPD and expands each SegmentTimeline into explicit
// per-segment entries.
func ParseDASH(data []byte) (*MPD, error) {
	var x xmlMPD
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("dash: %w", err)
	}
	if len(x.Periods) != 1 || len(x.Periods[0].AdaptationSets) != 1 {
		return nil, fmt.Errorf("dash: exactly one Period with one AdaptationSet is supported, got %d/%d",
			len(x.Periods), len(x.Periods[0].AdaptationSets))
	}
	p := x.Periods[0]
	out := &MPD{
		Type:                      x.Type,
		MediaPresentationDuration: x.MediaPresentationDuration,
		PeriodID:                  p.ID,
		PeriodDuration:            p.Duration,
	}
	for _, r := range p.AdaptationSets[0].Representations {
		st := r.SegmentTemplate
		if st.Timescale == 0 {
			return nil, fmt.Errorf("dash: representation %q: timescale must be > 0", r.ID)
		}
		rep := MPDRepresentation{
			ID:          r.ID,
			Bandwidth:   r.Bandwidth,
			Timescale:   st.Timescale,
			PTO:         st.PTO,
			StartNumber: st.StartNumber,
			Media:       st.Media,
		}
		var next uint64
		for i, s := range st.S {
			t := next
			if s.T != nil {
				t = *s.T
			}
			if s.D == 0 {
				return nil, fmt.Errorf("dash: representation %q: S[%d] has zero duration", r.ID, i)
			}
			repeat := 0
			if s.R != nil {
				repeat = *s.R
				if repeat < 0 {
					return nil, fmt.Errorf("dash: representation %q: negative S@r unsupported", r.ID)
				}
			}
			for j := 0; j <= repeat; j++ {
				rep.Segments = append(rep.Segments, MPDSegment{T: t, D: s.D})
				t += s.D
			}
			next = t
		}
		out.Representations = append(out.Representations, rep)
	}
	return out, nil
}

// SegmentURL resolves the media template for segment i of the
// representation ($RepresentationID$ and $Number$ tokens).
func (r MPDRepresentation) SegmentURL(i int) string {
	u := strings.ReplaceAll(r.Media, "$RepresentationID$", r.ID)
	u = strings.ReplaceAll(u, "$Number$", fmt.Sprint(r.StartNumber+uint64(i)))
	return u
}

// StartSeconds maps a segment's presentation time onto the unified
// timeline: (t - pto) / timescale. A time below the offset is a conflict
// reported by the caller.
func (r MPDRepresentation) StartSeconds(s MPDSegment) core.Rat {
	return core.RatFromUnits(int64(s.T)-int64(r.PTO), int64(r.Timescale))
}

// DurationSeconds converts a segment duration to seconds.
func (r MPDRepresentation) DurationSeconds(s MPDSegment) core.Rat {
	return core.RatFromUnits(int64(s.D), int64(r.Timescale))
}

// EncodeDASH serializes the normalized MPD with an explicit (unrolled)
// SegmentTimeline, which makes the encoding canonical.
func EncodeDASH(m *MPD) ([]byte, error) {
	x := xmlMPD{
		Type:                      m.Type,
		MediaPresentationDuration: m.MediaPresentationDuration,
	}
	p := xmlPeriod{ID: m.PeriodID, Duration: m.PeriodDuration}
	as := xmlAdaptationSet{ID: "1"}
	for _, r := range m.Representations {
		xr := xmlRepresentation{ID: r.ID, Bandwidth: r.Bandwidth}
		xr.SegmentTemplate = xmlSegmentTemplate{
			Timescale:   r.Timescale,
			PTO:         r.PTO,
			StartNumber: r.StartNumber,
			Media:       r.Media,
		}
		for _, s := range r.Segments {
			t := s.T
			xr.SegmentTemplate.S = append(xr.SegmentTemplate.S, xmlS{T: &t, D: s.D})
		}
		as.Representations = append(as.Representations, xr)
	}
	p.AdaptationSets = []xmlAdaptationSet{as}
	x.Periods = []xmlPeriod{p}
	out, err := xml.MarshalIndent(x, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

// diffDASH returns the first differing field, or "" when equal.
func diffDASH(a, b *MPD) string {
	if a.Type != b.Type {
		return "type"
	}
	if a.MediaPresentationDuration != b.MediaPresentationDuration {
		return "media-presentation-duration"
	}
	if a.PeriodID != b.PeriodID {
		return "period.id"
	}
	if a.PeriodDuration != b.PeriodDuration {
		return "period.duration"
	}
	if len(a.Representations) != len(b.Representations) {
		return "representations.count"
	}
	for i := range a.Representations {
		x, y := a.Representations[i], b.Representations[i]
		field := fmt.Sprintf("representations[%d]", i)
		switch {
		case x.ID != y.ID:
			return field + ".id"
		case x.Bandwidth != y.Bandwidth:
			return field + ".bandwidth"
		case x.Timescale != y.Timescale:
			return field + ".timescale"
		case x.PTO != y.PTO:
			return field + ".presentation-time-offset"
		case x.StartNumber != y.StartNumber:
			return field + ".start-number"
		case x.Media != y.Media:
			return field + ".media"
		case len(x.Segments) != len(y.Segments):
			return field + ".segments.count"
		}
		for j := range x.Segments {
			if x.Segments[j] != y.Segments[j] {
				return fmt.Sprintf("%s.segments[%d]", field, j)
			}
		}
	}
	return ""
}

// RoundTripDASH parses, re-encodes and re-parses an MPD, comparing
// normalized semantics. The first differing field is reported.
func RoundTripDASH(name string, data []byte) (*MPD, error) {
	a, err := ParseDASH(data)
	if err != nil {
		return nil, err
	}
	enc, err := EncodeDASH(a)
	if err != nil {
		return nil, err
	}
	b, err := ParseDASH(enc)
	if err != nil {
		return nil, fmt.Errorf("dash: re-parse after encode: %w", err)
	}
	if f := diffDASH(a, b); f != "" {
		return nil, &MismatchError{Manifest: name, Field: f}
	}
	return a, nil
}
