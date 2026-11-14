// Package mpd defines the self-contained DASH manifest model: Period,
// AdaptationSet, Representation, SegmentTemplate and SegmentTimeline.
package mpd

import "encoding/xml"

// MPD is the root of the media presentation description.
type MPD struct {
	XMLName                   xml.Name `xml:"MPD" json:"-"`
	XMLNS                     string   `xml:"xmlns,attr" json:"xmlns"`
	Profiles                  string   `xml:"profiles,attr" json:"profiles"`
	Type                      string   `xml:"type,attr" json:"type"`
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr" json:"mediaPresentationDuration"`
	MinBufferTime             string   `xml:"minBufferTime,attr" json:"minBufferTime"`
	Period                    Period   `xml:"Period" json:"period"`
}

// Period groups the adaptation sets of the presentation.
type Period struct {
	ID             string          `xml:"id,attr" json:"id"`
	Duration       string          `xml:"duration,attr" json:"duration"`
	AdaptationSets []AdaptationSet `xml:"AdaptationSet" json:"adaptationSets"`
}

// AdaptationSet groups representations of one content type.
type AdaptationSet struct {
	ID              int              `xml:"id,attr" json:"id"`
	ContentType     string           `xml:"contentType,attr" json:"contentType"`
	Representations []Representation `xml:"Representation" json:"representations"`
}

// Representation is one quality level with its own segment template.
type Representation struct {
	ID              string          `xml:"id,attr" json:"id"`
	Bandwidth       uint64          `xml:"bandwidth,attr" json:"bandwidth"`
	SegmentTemplate SegmentTemplate `xml:"SegmentTemplate" json:"segmentTemplate"`
}

// SegmentTemplate carries addressing and timing for one representation.
type SegmentTemplate struct {
	Timescale              uint64           `xml:"timescale,attr" json:"timescale"`
	Duration               uint64           `xml:"duration,attr" json:"duration"`
	StartNumber            uint64           `xml:"startNumber,attr" json:"startNumber"`
	PresentationTimeOffset uint64           `xml:"presentationTimeOffset,attr" json:"presentationTimeOffset"`
	Media                  string           `xml:"media,attr" json:"media"`
	Initialization         string           `xml:"initialization,attr" json:"initialization"`
	SegmentTimeline        *SegmentTimeline `xml:"SegmentTimeline" json:"segmentTimeline,omitempty"`
}

// SegmentTimeline is the explicit list of segment timings.
type SegmentTimeline struct {
	S []S `xml:"S" json:"s"`
}

// S is one SegmentTimeline entry: start time, duration, repeat count.
type S struct {
	T uint64 `xml:"t,attr" json:"t"`
	D uint64 `xml:"d,attr" json:"d"`
	R int    `xml:"r,attr,omitempty" json:"r,omitempty"`
}

const (
	// Namespace is the DASH MPD namespace used in generated manifests.
	Namespace = "urn:mpeg:dash:schema:mpd:2011"
	// Profiles is the manifest profile string.
	Profiles = "urn:mpeg:dash:profile:isoff-live:2011"
)
