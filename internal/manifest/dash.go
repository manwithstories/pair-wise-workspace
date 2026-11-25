package manifest

import (
	"bytes"
	"encoding/xml"
	"math"
	"strconv"
	"strings"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

type dashMPD struct {
	XMLName xml.Name     `xml:"MPD"`
	Type    string       `xml:"type,attr"`
	Periods []dashPeriod `xml:"Period"`
}

type dashPeriod struct {
	ID             string              `xml:"id,attr"`
	AdaptationSets []dashAdaptationSet `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	ContentType  string               `xml:"contentType,attr"`
	MimeType     string               `xml:"mimeType,attr"`
	Codecs       string               `xml:"codecs,attr"`
	Width        int                  `xml:"width,attr"`
	Height       int                  `xml:"height,attr"`
	Template     *dashTemplate        `xml:"SegmentTemplate"`
	Representations []dashRepresentation `xml:"Representation"`
}

type dashRepresentation struct {
	ID        string        `xml:"id,attr"`
	Codecs    string        `xml:"codecs,attr"`
	MimeType  string        `xml:"mimeType,attr"`
	Width     int           `xml:"width,attr"`
	Height    int           `xml:"height,attr"`
	Bandwidth int           `xml:"bandwidth,attr"`
	Template  *dashTemplate `xml:"SegmentTemplate"`
}

type dashTemplate struct {
	Timescale         int64              `xml:"timescale,attr"`
	StartNumber       int64              `xml:"startNumber,attr"`
	Initialization    string             `xml:"initialization,attr"`
	Media             string             `xml:"media,attr"`
	SegmentTimeline   *dashTimelineXML   `xml:"SegmentTimeline"`
}

type dashTimelineXML struct {
	S []dashS `xml:"S"`
}

type dashS struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R *int   `xml:"r,attr"`
}

// ParseDASH parses MPD SegmentTimeline data without network access.
func ParseDASH(path string, data []byte) (*Manifest, error) {
	var root dashMPD
	dec := xml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&root); err != nil {
		off := dec.InputOffset()
		return nil, fault.New(fault.InvalidManifest, path, off, "invalid MPD XML: %v", err)
	}
	if root.XMLName.Local != "MPD" {
		return nil, fault.New(fault.InvalidManifest, path, 0, "missing MPD root")
	}

	m := &Manifest{Kind: "dash", Path: path, Version: 1}
	for periodIndex, period := range root.Periods {
		for _, as := range period.AdaptationSets {
			for _, repXML := range as.Representations {
				tmpl := mergeTemplate(as.Template, repXML.Template)
				codecs := firstNonEmpty(repXML.Codecs, as.Codecs)
				mime := firstNonEmpty(repXML.MimeType, as.MimeType)
				rep := Representation{
					ID: firstNonEmpty(repXML.ID, codecs), Codecs: codecs, MimeType: mime,
					Width: firstNonZero(repXML.Width, as.Width),
					Height: firstNonZero(repXML.Height, as.Height),
					Bandwidth: repXML.Bandwidth,
				}
				segs, err := expandDASHTemplate(path, tmpl, rep.ID, periodIndex)
				if err != nil {
					return nil, err
				}
				rep.Segments = segs
				m.Representations = append(m.Representations, rep)
			}
		}
	}
	if len(m.Representations) == 0 {
		return nil, fault.New(fault.InvalidManifest, path, 0, "MPD contains no representations")
	}
	// The first representation is the default flat view for simple callers.
	m.Segments = m.Representations[0].Segments
	return m, nil
}

func mergeTemplate(base, override *dashTemplate) *dashTemplate {
	if override == nil {
		return base
	}
	if base == nil {
		return override
	}
	out := *base
	if override.Timescale != 0 {
		out.Timescale = override.Timescale
	}
	if override.StartNumber != 0 {
		out.StartNumber = override.StartNumber
	}
	if override.Initialization != "" {
		out.Initialization = override.Initialization
	}
	if override.Media != "" {
		out.Media = override.Media
	}
	if override.SegmentTimeline != nil {
		out.SegmentTimeline = override.SegmentTimeline
	}
	return &out
}

func expandDASHTemplate(path string, t *dashTemplate, repID string, period int) ([]Segment, error) {
	if t == nil || t.SegmentTimeline == nil {
		return nil, fault.New(fault.InvalidManifest, path, 0, "representation %s lacks SegmentTimeline", repID)
	}
	if t.Timescale <= 0 {
		return nil, fault.New(fault.InvalidManifest, path, 0, "representation %s has invalid timescale", repID)
	}
	if t.Initialization == "" {
		return nil, fault.New(fault.InitMissing, path, 0, "representation %s lacks initialization template", repID)
	}
	if t.Media == "" {
		return nil, fault.New(fault.InvalidManifest, path, 0, "representation %s lacks media template", repID)
	}
	startNumber := t.StartNumber
	if startNumber == 0 {
		startNumber = 1
	}
	initURI := strings.ReplaceAll(t.Initialization, "$RepresentationID$", repID)
	var segments []Segment
	number := startNumber
	var cursor int64
	for _, item := range t.SegmentTimeline.S {
		if item.D <= 0 {
			return nil, fault.New(fault.Boundary, path, item.TValue(), "invalid SegmentTimeline duration d=%d", item.D)
		}
		if item.T != nil {
			cursor = *item.T
		}
		repeats := 0
		if item.R != nil {
			if *item.R < 0 {
				return nil, fault.New(fault.InvalidManifest, path, cursor, "negative open-ended SegmentTimeline repeats are unsupported in v1")
			}
			repeats = *item.R
		}
		dur, err := clock.New(item.D, t.Timescale)
		if err != nil {
			return nil, err
		}
		for j := 0; j <= repeats; j++ {
			start, err := clock.New(cursor, t.Timescale)
			if err != nil {
				return nil, err
			}
			uri := strings.ReplaceAll(t.Media, "$RepresentationID$", repID)
			uri = strings.ReplaceAll(uri, "$Number$", strconv.FormatInt(number, 10))
			segments = append(segments, Segment{
				Index: int(number - startNumber), Period: period, URI: uri, InitURI: initURI,
				Start: start, Duration: dur,
			})
			if cursor > math.MaxInt64-item.D {
				return nil, fault.New(fault.TimestampOverflow, path, cursor, "SegmentTimeline time overflow")
			}
			cursor += item.D
			number++
		}
	}
	if len(segments) == 0 {
		return nil, fault.New(fault.InvalidManifest, path, 0, "representation %s has empty SegmentTimeline", repID)
	}
	return segments, nil
}

func (s dashS) TValue() int64 {
	if s.T != nil {
		return *s.T
	}
	return -1
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
