package manifest

import (
	"encoding/xml"
	"errors"
	"strconv"
	"strings"

	"mediad/internal/clock"
)

// SerializeDASH renders the model as an MPD that re-parses identically.
func SerializeDASH(m *Manifest) ([]byte, error) {
	if m == nil || m.Kind != KindDASH {
		return nil, errors.New("manifest: not a DASH manifest")
	}
	canon := clock.Default()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"`)
	if m.DASH.Profiles != "" {
		b.WriteString(` profiles="` + xmlEscape(m.DASH.Profiles) + `"`)
	}
	if m.DASH.HasMPD {
		b.WriteString(` mediaPresentationDuration="` + clock.FormatISODurationTicks(m.DASH.MediaPresentationDuration, canon.Timescale) + `"`)
	}
	if m.DASH.HasMinBufferTime {
		b.WriteString(` minBufferTime="` + clock.FormatISODurationTicks(m.DASH.MinBufferTime, canon.Timescale) + `"`)
	}
	b.WriteString(">\n  <Period")
	if m.DASH.PeriodID != "" {
		b.WriteString(` id="` + xmlEscape(m.DASH.PeriodID) + `"`)
	}
	if m.DASH.HasPeriodStart {
		b.WriteString(` start="` + clock.FormatISODurationTicks(m.DASH.PeriodStart, canon.Timescale) + `"`)
	}
	if m.DASH.HasPeriodDuration {
		b.WriteString(` duration="` + clock.FormatISODurationTicks(m.DASH.PeriodDuration, canon.Timescale) + `"`)
	}
	b.WriteString(">\n    <AdaptationSet>\n")
	reps := make([]Representation, len(m.Representations))
	copy(reps, m.Representations)
	sortRepresentations(reps)
	for _, rep := range reps {
		b.WriteString(`      <Representation id="` + xmlEscape(rep.ID) + `" bandwidth="` + strconv.FormatInt(rep.Bandwidth, 10) + `"`)
		if rep.Codecs != "" {
			b.WriteString(` codecs="` + xmlEscape(rep.Codecs) + `"`)
		}
		if rep.MimeType != "" {
			b.WriteString(` mimeType="` + xmlEscape(rep.MimeType) + `"`)
		}
		if rep.Width != 0 {
			b.WriteString(` width="` + strconv.Itoa(rep.Width) + `"`)
		}
		if rep.Height != 0 {
			b.WriteString(` height="` + strconv.Itoa(rep.Height) + `"`)
		}
		b.WriteString(">\n")
		b.WriteString(templateXML(rep.Template))
		b.WriteString("      </Representation>\n")
	}
	b.WriteString("    </AdaptationSet>\n  </Period>\n</MPD>\n")
	return []byte(b.String()), nil
}

func sortRepresentations(reps []Representation) {
	for i := 1; i < len(reps); i++ {
		for j := i; j > 0 && reps[j].ID < reps[j-1].ID; j-- {
			reps[j], reps[j-1] = reps[j-1], reps[j]
		}
	}
}

func templateXML(t *Template) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("        <SegmentTemplate")
	b.WriteString(` media="` + xmlEscape(t.Media) + `"`)
	if t.Initialization != "" {
		b.WriteString(` initialization="` + xmlEscape(t.Initialization) + `"`)
	}
	b.WriteString(` timescale="` + strconv.FormatUint(uint64(t.Timescale), 10) + `"`)
	sn := t.StartNumber
	if !t.HasStartNumber {
		sn = 1
	}
	b.WriteString(` startNumber="` + strconv.FormatUint(sn, 10) + `"`)
	if t.HasDuration {
		b.WriteString(` duration="` + strconv.FormatInt(t.Duration, 10) + `"`)
	}
	if t.HasPTO {
		b.WriteString(` presentationTimeOffset="` + strconv.FormatInt(t.PresentationTimeOffset, 10) + `"`)
	}
	if len(t.Timeline) == 0 {
		b.WriteString("/>\n")
		return b.String()
	}
	b.WriteString(">\n          <SegmentTimeline>\n")
	for _, s := range t.Timeline {
		b.WriteString("            <S")
		if s.HasTime {
			b.WriteString(` t="` + strconv.FormatInt(s.Time, 10) + `"`)
		}
		b.WriteString(` d="` + strconv.FormatInt(s.Duration, 10) + `"`)
		b.WriteString(` r="` + strconv.FormatInt(s.Repeat, 10) + `"`)
		b.WriteString("/>\n")
	}
	b.WriteString("          </SegmentTimeline>\n        </SegmentTemplate>\n")
	return b.String()
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
