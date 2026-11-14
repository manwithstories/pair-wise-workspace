// Package mpd defines the DASH manifest (MPD) model shared by pack and
// verify, plus XML encode/decode helpers. All time values carried by the
// model are int64 ticks in the SegmentTemplate timescale; no floating point
// is used anywhere in the pipeline.
package mpd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
)

// Namespace is the DASH MPD schema namespace emitted on the root element.
const Namespace = "urn:mpeg:dash:schema:mpd:2011"

// ErrRoundTrip is returned when a model does not survive a
// marshal -> parse -> marshal cycle byte-identically.
var ErrRoundTrip = errors.New("mpd: XML round-trip mismatch")

type MPD struct {
	XMLName                   xml.Name `xml:"urn:mpeg:dash:schema:mpd:2011 MPD"`
	Profiles                  string   `xml:"profiles,attr"`
	Type                      string   `xml:"type,attr"`
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr"`
	MinBufferTime             string   `xml:"minBufferTime,attr"`
	Periods                   []Period `xml:"Period"`
}

type Period struct {
	ID             string          `xml:"id,attr"`
	Start          string          `xml:"start,attr"`
	AdaptationSets []AdaptationSet `xml:"AdaptationSet"`
}

// AdaptationSet carries exactly one Representation per quality tier; tiers
// are ordered by ascending bandwidth.
type AdaptationSet struct {
	ID              int              `xml:"id,attr"`
	MimeType        string           `xml:"mimeType,attr"`
	ContentType     string           `xml:"contentType,attr"`
	Representations []Representation `xml:"Representation"`
}

type Representation struct {
	ID              string          `xml:"id,attr"`
	Bandwidth       int64           `xml:"bandwidth,attr"`
	Width           int             `xml:"width,attr"`
	Height          int             `xml:"height,attr"`
	BaseURL         string          `xml:"BaseURL"`
	SegmentTemplate SegmentTemplate `xml:"SegmentTemplate"`
}

type SegmentTemplate struct {
	Timescale              int64            `xml:"timescale,attr"`
	PresentationTimeOffset int64            `xml:"presentationTimeOffset,attr"`
	Media                  string           `xml:"media,attr"`
	StartNumber            *int64           `xml:"startNumber,attr,omitempty"`
	SegmentTimeline        *SegmentTimeline `xml:"SegmentTimeline"`
}

type SegmentTimeline struct {
	S []S `xml:"S"`
}

// S is one SegmentTimeline run. T is nil when it continues from the previous
// run; R is nil when the run holds a single segment.
type S struct {
	T *int64 `xml:"t,attr,omitempty"`
	D int64  `xml:"d,attr"`
	R *int64 `xml:"r,attr,omitempty"`
}

// Marshal renders the model as indented XML with a deterministic header.
func Marshal(m *MPD) ([]byte, error) {
	body, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mpd: marshal: %w", err)
	}
	out := make([]byte, 0, len(body)+len(xml.Header)+1)
	out = append(out, xml.Header...)
	out = append(out, body...)
	out = append(out, '\n')
	return out, nil
}

// Parse decodes an MPD document and applies minimal structural validation.
func Parse(data []byte) (*MPD, error) {
	var m MPD
	if err := xml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("mpd: parse: %w", err)
	}
	if len(m.Periods) == 0 {
		return nil, errors.New("mpd: document has no Period")
	}
	return &m, nil
}

// CheckRoundTrip verifies that the model survives a marshal/parse/marshal
// cycle byte-identically, guarding against lossy XML mappings.
func CheckRoundTrip(m *MPD) error {
	first, err := Marshal(m)
	if err != nil {
		return err
	}
	again, err := Parse(first)
	if err != nil {
		return err
	}
	second, err := Marshal(again)
	if err != nil {
		return err
	}
	if !bytes.Equal(first, second) {
		return ErrRoundTrip
	}
	return nil
}

// Representations flattens all Representations across AdaptationSets in
// document order.
func (m *MPD) Representations() []Representation {
	var out []Representation
	for _, p := range m.Periods {
		for _, as := range p.AdaptationSets {
			out = append(out, as.Representations...)
		}
	}
	return out
}

// FormatDuration renders whole seconds as an ISO 8601 duration.
func FormatDuration(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	out := "PT"
	if h > 0 {
		out += fmt.Sprintf("%dH", h)
	}
	if m > 0 {
		out += fmt.Sprintf("%dM", m)
	}
	if s > 0 || (h == 0 && m == 0) {
		out += fmt.Sprintf("%dS", s)
	}
	return out
}
