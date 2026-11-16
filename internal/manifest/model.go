package manifest

import (
	"encoding/xml"
	"fmt"
)

// Error is the shared coded error used at command boundaries.
type Error struct {
	Code    string
	Message string
	Exit    int
	Details any
}

func (e *Error) Error() string { return e.Message }

func NewError(code, message string, exit int, details any) *Error {
	return &Error{Code: code, Message: message, Exit: exit, Details: details}
}

type MPD struct {
	XMLName                   xml.Name `xml:"MPD"`
	XMLNS                     string   `xml:"xmlns,attr,omitempty"`
	Type                      string   `xml:"type,attr,omitempty"`
	Profiles                  string   `xml:"profiles,attr,omitempty"`
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr,omitempty"`
	MinBufferTime             string   `xml:"minBufferTime,attr,omitempty"`
	Periods                   []Period `xml:"Period"`
}

type Period struct {
	ID             string          `xml:"id,attr,omitempty"`
	Start          string          `xml:"start,attr,omitempty"`
	AdaptationSets []AdaptationSet `xml:"AdaptationSet"`
}

type AdaptationSet struct {
	ID              string           `xml:"id,attr,omitempty"`
	ContentType     string           `xml:"contentType,attr,omitempty"`
	MimeType        string           `xml:"mimeType,attr,omitempty"`
	Lang            string           `xml:"lang,attr,omitempty"`
	Representations []Representation `xml:"Representation"`
}

type Representation struct {
	ID                string           `xml:"id,attr"`
	Bandwidth         int64            `xml:"bandwidth,attr"`
	Codecs            string           `xml:"codecs,attr,omitempty"`
	Width             int64            `xml:"width,attr,omitempty"`
	Height            int64            `xml:"height,attr,omitempty"`
	AudioSamplingRate int64            `xml:"audioSamplingRate,attr,omitempty"`
	SegmentTemplate   *SegmentTemplate `xml:"SegmentTemplate,omitempty"`
}

type SegmentTemplate struct {
	Timescale              int64            `xml:"timescale,attr,omitempty"`
	PresentationTimeOffset int64            `xml:"presentationTimeOffset,attr,omitempty"`
	StartNumber            int64            `xml:"startNumber,attr,omitempty"`
	Media                  string           `xml:"media,attr"`
	Initialization         string           `xml:"initialization,attr,omitempty"`
	Timeline               *SegmentTimeline `xml:"SegmentTimeline,omitempty"`
}

type SegmentTimeline struct {
	Segments []S `xml:"S"`
}

type S struct {
	Time     *int64 `xml:"t,attr,omitempty"`
	Duration int64  `xml:"d,attr"`
	Repeat   int64  `xml:"r,attr,omitempty"`
}

func (m *MPD) AllRepresentations() []*Representation {
	var result []*Representation
	for i := range m.Periods {
		for j := range m.Periods[i].AdaptationSets {
			for k := range m.Periods[i].AdaptationSets[j].Representations {
				result = append(result, &m.Periods[i].AdaptationSets[j].Representations[k])
			}
		}
	}
	return result
}

func (m *MPD) Validate() error {
	if m.Type == "" {
		return NewError("MPD_INVALID", "manifest type is required", 4, nil)
	}
	if len(m.Periods) == 0 {
		return NewError("MPD_INVALID", "at least one period is required", 4, nil)
	}
	seen := make(map[string]struct{})
	for _, repr := range m.AllRepresentations() {
		if repr.ID == "" {
			return NewError("MPD_INVALID", "representation id is required", 4, nil)
		}
		if _, exists := seen[repr.ID]; exists {
			return NewError("DUPLICATE_REPRESENTATION",
				fmt.Sprintf("duplicate representation id %q", repr.ID), 4, repr.ID)
		}
		seen[repr.ID] = struct{}{}
		if repr.SegmentTemplate == nil {
			return NewError("MPD_INVALID",
				fmt.Sprintf("representation %q has no SegmentTemplate", repr.ID), 4, repr.ID)
		}
		ts := repr.SegmentTemplate.Timescale
		if ts <= 0 {
			return NewError("MPD_INVALID",
				fmt.Sprintf("representation %q has invalid timescale", repr.ID), 4, repr.ID)
		}
		if repr.SegmentTemplate.Timeline == nil ||
			len(repr.SegmentTemplate.Timeline.Segments) == 0 {
			return NewError("MPD_INVALID",
				fmt.Sprintf("representation %q has no SegmentTimeline", repr.ID), 4, repr.ID)
		}
		for _, segment := range repr.SegmentTemplate.Timeline.Segments {
			if segment.Duration <= 0 {
				return NewError("MPD_INVALID",
					fmt.Sprintf("representation %q has non-positive duration", repr.ID), 4, repr.ID)
			}
		}
	}
	return nil
}
