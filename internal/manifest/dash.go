package manifest

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"avlab/internal/media"
)

const dashNS = "urn:mpeg:dash:schema:mpd:2011"

type xmlMPD struct {
	XMLName       xml.Name        `xml:"MPD"`
	XMLNS         string          `xml:"xmlns,attr"`
	Type          string          `xml:"type,attr"`
	MinBufferTime string          `xml:"minBufferTime,attr"`
	Period        xmlPeriod       `xml:"Period"`
}

type xmlPeriod struct {
	ID           string           `xml:"id,attr"`
	Adaptation   xmlAdaptationSet `xml:"AdaptationSet"`
}

type xmlAdaptationSet struct {
	SegmentAlignment bool                `xml:"segmentAlignment,attr"`
	ContentType      string              `xml:"contentType,attr"`
	Representations  []xmlRepresentation `xml:"Representation"`
}

type xmlRepresentation struct {
	ID          string         `xml:"id,attr"`
	Bandwidth   uint32         `xml:"bandwidth,attr"`
	Width       uint32         `xml:"width,attr"`
	Height      uint32         `xml:"height,attr"`
	MimeType    string         `xml:"mimeType,attr"`
	SegmentList xmlSegmentList `xml:"SegmentList"`
}

type xmlSegmentList struct {
	Timescale   uint32          `xml:"timescale,attr"`
	Duration    int64           `xml:"duration,attr"`
	StartNumber uint32          `xml:"startNumber,attr"`
	URLs        []xmlSegmentURL `xml:"SegmentURL"`
}

type xmlSegmentURL struct {
	Media string `xml:"media,attr"`
}

// ParseDASH decodes an MPD document into the normalized Manifest.
func ParseDASH(data []byte) (*Manifest, error) {
	if err := checkSize(len(data)); err != nil {
		return nil, err
	}
	var doc xmlMPD
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, media.WrapFault(media.CodeManifestDamaged, false,
			"MPD is not well-formed XML", err)
	}
	if doc.XMLNS != dashNS || doc.XMLName.Local != "MPD" {
		return nil, media.NewFault(media.CodeManifestDamaged, false,
			"missing DASH namespace or MPD root element")
	}
	minBuf, err := parsePTSeconds(doc.MinBufferTime)
	if err != nil {
		return nil, media.WrapFault(media.CodeManifestDamaged, false,
			"invalid minBufferTime", err)
	}
	m := &Manifest{
		Kind: KindDASH,
		MinBufferTick: media.FromDuration(
			time.Duration(minBuf * float64(time.Second))),
		Representations: make([]Representation, 0,
			len(doc.Period.Adaptation.Representations)),
	}
	for _, xr := range doc.Period.Adaptation.Representations {
		rep, err := dashRep(xr)
		if err != nil {
			return nil, err
		}
		if m.TargetTick == 0 {
			m.TargetTick = rep.Segments[0].DurationTick
		}
		m.Representations = append(m.Representations, rep)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func dashRep(xr xmlRepresentation) (Representation, error) {
	if xr.SegmentList.Timescale != media.ClockRate {
		return Representation{}, media.NewFault(media.CodeManifestDamaged, false,
			"SegmentList timescale must be 90000")
	}
	if xr.SegmentList.Duration <= 0 {
		return Representation{}, media.NewFault(media.CodeManifestDamaged, false,
			"SegmentList duration must be positive")
	}
	rep := Representation{
		ID: xr.ID, Bandwidth: xr.Bandwidth, Width: xr.Width,
		Height: xr.Height, MIME: xr.MimeType,
		Segments: make([]Segment, 0, len(xr.SegmentList.URLs)),
	}
	var start media.MediaTime
	for i, u := range xr.SegmentList.URLs {
		if u.Media == "" {
			return rep, media.NewFault(media.CodeManifestDamaged, false,
				"SegmentURL without media attribute")
		}
		rep.Segments = append(rep.Segments, Segment{
			Index:        xr.SegmentList.StartNumber + uint32(i),
			URL:          u.Media,
			StartTick:    start,
			DurationTick: media.MediaTime(xr.SegmentList.Duration),
			Epoch:        0,
			Keyframe:     true,
		})
		next, err := start.Add(media.MediaTime(xr.SegmentList.Duration))
		if err != nil {
			return rep, media.WrapFault(media.CodeTimestampOverflow, false,
				"segment timeline overflow", err)
		}
		start = next
	}
	return rep, nil
}

// RenderDASH serializes a normalized Manifest back to a deterministic MPD.
func RenderDASH(m *Manifest) ([]byte, error) {
	if m == nil || m.Kind != KindDASH {
		return nil, media.NewFault(media.CodeInvalidArgument, false,
			"RenderDASH requires a DASH manifest")
	}
	doc := xmlMPD{
		XMLNS: dashNS, Type: "static",
		MinBufferTime: formatPTSeconds(m.MinBufferTick.Duration()),
		Period:        xmlPeriod{ID: "0"},
	}
	doc.Period.Adaptation = xmlAdaptationSet{
		SegmentAlignment: true, ContentType: "video",
		Representations: make([]xmlRepresentation, 0, len(m.Representations)),
	}
	for _, r := range m.Representations {
		xr := xmlRepresentation{
			ID: r.ID, Bandwidth: r.Bandwidth, Width: r.Width, Height: r.Height,
			MimeType: r.MIME,
			SegmentList: xmlSegmentList{
				Timescale:   media.ClockRate,
				Duration:    int64(r.Segments[0].DurationTick),
				StartNumber: r.Segments[0].Index,
				URLs:        make([]xmlSegmentURL, 0, len(r.Segments)),
			},
		}
		for _, s := range r.Segments {
			xr.SegmentList.URLs = append(xr.SegmentList.URLs,
				xmlSegmentURL{Media: s.URL})
		}
		doc.Period.Adaptation.Representations =
			append(doc.Period.Adaptation.Representations, xr)
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(&doc); err != nil {
		return nil, media.WrapFault(media.CodeInternal, false, "MPD encode failed", err)
	}
	return buf.Bytes(), nil
}

// parsePTSeconds parses the restricted ISO-8601 duration form PT<n>S.
func parsePTSeconds(s string) (d float64, err error) {
	if !strings.HasPrefix(s, "PT") || !strings.HasSuffix(s, "S") {
		return 0, fmt.Errorf("unsupported duration %q", s)
	}
	d, err = strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(s, "PT"), "S"), 64)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

func formatPTSeconds(d time.Duration) string {
	return "PT" + strconv.FormatFloat(float64(d.Milliseconds())/1000, 'f', 3, 64) + "S"
}
