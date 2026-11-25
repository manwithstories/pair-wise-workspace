// Package manifest parses, normalizes, and canonicalizes DASH and HLS manifests.
package manifest

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pairwise/mediad/internal/media"
)

// Mismatch identifies the first field that differs after parse/serialize.
type Mismatch struct {
	Field string `json:"field"`
	Want  string `json:"want"`
	Got   string `json:"got"`
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s: want=%s got=%s", m.Field, m.Want, m.Got)
}

// DASHManifest is the small, deterministic DASH profile used by mediad.
type DASHManifest struct {
	XMLName                   xml.Name        `xml:"MPD"`
	XMLNS                     string          `xml:"xmlns,attr"`
	Profiles                  string          `xml:"profiles,attr,omitempty"`
	Type                      string          `xml:"type,attr"`
	MediaPresentationDuration string          `xml:"mediaPresentationDuration,attr"`
	MinBufferTime             string          `xml:"minBufferTime,attr"`
	Period                    DASHPeriod      `xml:"Period"`
}

// DASHPeriod contains one adaptation set in the fixture profile.
type DASHPeriod struct {
	ID            string                `xml:"id,attr,omitempty"`
	Duration      string                `xml:"duration,attr,omitempty"`
	AdaptationSet DASHAdaptationSet     `xml:"AdaptationSet"`
}

// DASHAdaptationSet groups representations.
type DASHAdaptationSet struct {
	ContentType string             `xml:"contentType,attr"`
	SegmentTpl  DASHSegmentTpl     `xml:"SegmentTemplate"`
	Representations []DASHRepresentation `xml:"Representation"`
}

// DASHSegmentTpl is the common segment template.
type DASHSegmentTpl struct {
	Timescale      int    `xml:"timescale,attr"`
	Initialization string `xml:"initialization,attr"`
	Media          string `xml:"media,attr"`
	StartNumber    int    `xml:"startNumber,attr"`
	Duration       int    `xml:"duration,attr"`
}

// DASHRepresentation is one bitrate tier.
type DASHRepresentation struct {
	ID                string `xml:"id,attr"`
	Bandwidth         int    `xml:"bandwidth,attr"`
	Width             int    `xml:"width,attr"`
	Height            int    `xml:"height,attr"`
	Codecs            string `xml:"codecs,attr"`
	MimeType          string `xml:"mimeType,attr"`
	SAR               string `xml:"sar,attr,omitempty"`
}

// ParseDASH parses and validates required fields.
func ParseDASH(data []byte) (*DASHManifest, error) {
	var m DASHManifest
	if err := xml.Unmarshal(data, &m); err != nil {
		return nil, media.Wrap(err, media.CodeMalformed, "manifest.dash", "$", "invalid XML")
	}
	if m.XMLName.Local != "MPD" {
		return nil, media.NewError(media.CodeMalformed, "manifest.dash", "MPD", "missing MPD root")
	}
	m.Type = strings.TrimSpace(m.Type)
	m.MediaPresentationDuration = strings.TrimSpace(m.MediaPresentationDuration)
	m.MinBufferTime = strings.TrimSpace(m.MinBufferTime)
	if m.Type == "" {
		return nil, media.NewError(media.CodeMissingField, "manifest.dash", "MPD.type", "required attribute missing")
	}
	if m.MediaPresentationDuration == "" {
		return nil, media.NewError(media.CodeMissingField, "manifest.dash", "MPD.mediaPresentationDuration", "required attribute missing")
	}
	as := &m.Period.AdaptationSet
	if as.ContentType == "" {
		return nil, media.NewError(media.CodeMissingField, "manifest.dash", "AdaptationSet.contentType", "required attribute missing")
	}
	if as.SegmentTpl.Timescale <= 0 {
		return nil, media.NewError(media.CodeMissingField, "manifest.dash", "SegmentTemplate.timescale", "must be positive")
	}
	if as.SegmentTpl.Media == "" || as.SegmentTpl.Initialization == "" {
		return nil, media.NewError(media.CodeMissingField, "manifest.dash", "SegmentTemplate.media", "media and initialization templates required")
	}
	if len(as.Representations) == 0 {
		return nil, media.NewError(media.CodeMissingField, "manifest.dash", "Representation", "at least one representation required")
	}
	for i := range as.Representations {
		r := &as.Representations[i]
		r.ID = strings.TrimSpace(r.ID)
		r.Codecs = strings.TrimSpace(r.Codecs)
		if r.ID == "" {
			return nil, media.NewError(media.CodeMissingField, "manifest.dash", "Representation.id", "required", )
		}
		if r.Bandwidth <= 0 {
			return nil, media.NewError(media.CodeMissingField, "manifest.dash", fmt.Sprintf("Representation[%s]@bandwidth", r.ID), "must be positive")
		}
		if r.Width <= 0 || r.Height <= 0 {
			return nil, media.NewError(media.CodeMissingField, "manifest.dash", fmt.Sprintf("Representation[%s]@width/height", r.ID), "dimensions must be positive")
		}
		if r.Codecs == "" {
			return nil, media.NewError(media.CodeMissingField, "manifest.dash", fmt.Sprintf("Representation[%s]@codecs", r.ID), "required")
		}
	}
	return &m, nil
}

// NormalizeDASH trims values and orders representations by stable bandwidth/id.
func NormalizeDASH(m *DASHManifest) *DASHManifest {
	m.XMLNS = "urn:mpeg:dash:schema:mpd:2011"
	if m.Profiles == "" {
		m.Profiles = "urn:mpeg:dash:profile:isoff-live:2012"
	}
	sort.SliceStable(m.Period.AdaptationSet.Representations, func(i, j int) bool {
		a, b := m.Period.AdaptationSet.Representations[i], m.Period.AdaptationSet.Representations[j]
		if a.Bandwidth != b.Bandwidth {
			return a.Bandwidth < b.Bandwidth
		}
		return a.ID < b.ID
	})
	return m
}

// SerializeDASH returns canonical indented XML ending with a newline.
func SerializeDASH(m *DASHManifest) ([]byte, error) {
	NormalizeDASH(m)
	out, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, media.Wrap(err, media.CodeMalformed, "manifest.dash", "$", "canonical serialization failed")
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

// CompareDASH returns the first semantic field mismatch in a stable order.
func CompareDASH(want, got *DASHManifest) *Mismatch {
	checks := []struct {
		field string
		w, g  string
	}{
		{"MPD.type", want.Type, got.Type},
		{"MPD.mediaPresentationDuration", want.MediaPresentationDuration, got.MediaPresentationDuration},
		{"MPD.minBufferTime", want.MinBufferTime, got.MinBufferTime},
		{"Period.id", want.Period.ID, got.Period.ID},
		{"AdaptationSet.contentType", want.Period.AdaptationSet.ContentType, got.Period.AdaptationSet.ContentType},
		{"SegmentTemplate.timescale", strconv.Itoa(want.Period.AdaptationSet.SegmentTpl.Timescale), strconv.Itoa(got.Period.AdaptationSet.SegmentTpl.Timescale)},
		{"SegmentTemplate.media", want.Period.AdaptationSet.SegmentTpl.Media, got.Period.AdaptationSet.SegmentTpl.Media},
		{"Representation.count", strconv.Itoa(len(want.Period.AdaptationSet.Representations)), strconv.Itoa(len(got.Period.AdaptationSet.Representations))},
	}
	for _, c := range checks {
		if c.w != c.g {
			return &Mismatch{Field: c.field, Want: c.w, Got: c.g}
		}
	}
	n := min(len(want.Period.AdaptationSet.Representations), len(got.Period.AdaptationSet.Representations))
	for i := 0; i < n; i++ {
		w, g := want.Period.AdaptationSet.Representations[i], got.Period.AdaptationSet.Representations[i]
		prefix := fmt.Sprintf("Representation[%d]", i)
		for _, c := range []struct {
			f, w, g string
		}{
			{prefix + ".id", w.ID, g.ID},
			{prefix + ".bandwidth", strconv.Itoa(w.Bandwidth), strconv.Itoa(g.Bandwidth)},
			{prefix + ".width", strconv.Itoa(w.Width), strconv.Itoa(g.Width)},
			{prefix + ".height", strconv.Itoa(w.Height), strconv.Itoa(g.Height)},
			{prefix + ".codecs", w.Codecs, g.Codecs},
		} {
			if c.w != c.g {
				return &Mismatch{Field: c.f, Want: c.w, Got: c.g}
			}
		}
	}
	return nil
}

// RoundTripDASH parses, normalizes, serializes, and reparses a manifest.
func RoundTripDASH(data []byte) (canonical []byte, original, reparsed *DASHManifest, mismatch *Mismatch, err error) {
	original, err = ParseDASH(data)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	canonical, err = SerializeDASH(original)
	if err != nil {
		return nil, original, nil, nil, err
	}
	reparsed, err = ParseDASH(canonical)
	if err != nil {
		return canonical, original, nil, nil, err
	}
	mismatch = CompareDASH(NormalizeDASH(original), NormalizeDASH(reparsed))
	return canonical, original, reparsed, mismatch, nil
}
