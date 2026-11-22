package manifest

import "encoding/xml"

// The structs mirror the MPD subset that matters for segment expansion.
// Pointers distinguish absent attributes from zero values, which is required
// for correct inheritance and round trips.

type xmlMPD struct {
	XMLName                   xml.Name    `xml:"MPD"`
	Profiles                  string      `xml:"profiles,attr,omitempty"`
	MinBufferTime             string      `xml:"minBufferTime,attr,omitempty"`
	MediaPresentationDuration string      `xml:"mediaPresentationDuration,attr,omitempty"`
	Periods                   []xmlPeriod `xml:"Period"`
}

type xmlPeriod struct {
	ID             string             `xml:"id,attr,omitempty"`
	Start          string             `xml:"start,attr,omitempty"`
	Duration       string             `xml:"duration,attr,omitempty"`
	AdaptationSets []xmlAdaptationSet `xml:"AdaptationSet"`
}

type xmlAdaptationSet struct {
	MimeType        string              `xml:"mimeType,attr,omitempty"`
	Codecs          string              `xml:"codecs,attr,omitempty"`
	Width           int                 `xml:"width,attr,omitempty"`
	Height          int                 `xml:"height,attr,omitempty"`
	Template        *xmlTemplate        `xml:"SegmentTemplate"`
	Representations []xmlRepresentation `xml:"Representation"`
}

type xmlRepresentation struct {
	ID        string       `xml:"id,attr,omitempty"`
	Bandwidth int64        `xml:"bandwidth,attr,omitempty"`
	Codecs    string       `xml:"codecs,attr,omitempty"`
	MimeType  string       `xml:"mimeType,attr,omitempty"`
	Width     int          `xml:"width,attr,omitempty"`
	Height    int          `xml:"height,attr,omitempty"`
	Template  *xmlTemplate `xml:"SegmentTemplate"`
}

type xmlTemplate struct {
	Media                  *string      `xml:"media,attr"`
	Initialization         *string      `xml:"initialization,attr"`
	Timescale              *uint32      `xml:"timescale,attr"`
	Duration               *int64       `xml:"duration,attr"`
	StartNumber            *uint64      `xml:"startNumber,attr"`
	PresentationTimeOffset *int64       `xml:"presentationTimeOffset,attr"`
	Timeline               *xmlTimeline `xml:"SegmentTimeline"`
}

type xmlTimeline struct {
	Entries []xmlS `xml:"S"`
}

type xmlS struct {
	Time     *int64 `xml:"t,attr"`
	Duration int64  `xml:"d,attr"`
	Repeat   *int64 `xml:"r,attr"`
}

func strPtr(s string) *string { return &s }

// mergeTemplate overlays child attributes on parent. SegmentTimeline is
// replaced wholesale when present, matching DASH inheritance rules.
func mergeTemplate(parent, child *xmlTemplate) *Template {
	if parent == nil && child == nil {
		return nil
	}
	t := &Template{StartNumber: 1}
	apply := func(src *xmlTemplate) {
		if src == nil {
			return
		}
		if src.Media != nil {
			t.Media = *src.Media
		}
		if src.Initialization != nil {
			t.Initialization = *src.Initialization
		}
		if src.Timescale != nil {
			t.Timescale = *src.Timescale
		}
		if src.Duration != nil {
			t.Duration, t.HasDuration = *src.Duration, true
		}
		if src.StartNumber != nil {
			t.StartNumber, t.HasStartNumber = *src.StartNumber, true
		}
		if src.PresentationTimeOffset != nil {
			t.PresentationTimeOffset, t.HasPTO = *src.PresentationTimeOffset, true
		}
		if src.Timeline != nil {
			t.Timeline = convertTimeline(src.Timeline)
		}
	}
	apply(parent)
	apply(child)
	if t.Timescale == 0 {
		t.Timescale = 1
	}
	return t
}

func convertTimeline(x *xmlTimeline) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(x.Entries))
	for _, s := range x.Entries {
		e := TimelineEntry{Duration: s.Duration}
		if s.Time != nil {
			e.Time, e.HasTime = *s.Time, true
		}
		if s.Repeat != nil {
			e.Repeat = *s.Repeat
		}
		out = append(out, e)
	}
	return out
}

// isDASHDocument reports whether data looks like an MPD document.
func isDASHDocument(data []byte) bool {
	var probe struct {
		XMLName xml.Name
	}
	return xml.Unmarshal(data, &probe) == nil && probe.XMLName.Local == "MPD"
}
