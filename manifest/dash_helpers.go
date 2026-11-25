package manifest

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"fieldpost/internal/core"
)

// DASHTemplate describes a static SegmentTemplate.
type DASHTemplate struct {
	Timescale      uint32
	Duration       int64
	StartNumber    int64
	Initialization string
	Media          string
	Offset         int64
	Timeline       []DASHSpan
}

// DASHSpan is one SegmentTimeline S entry.
type DASHSpan struct {
	N int64
	D int64
	R int64
}

// DASHRepresentation contains one representation's segment location rules.
type DASHRepresentation struct {
	ID        string
	MimeType  string
	Codecs    string
	Bandwidth int
	Offset    int64
	Template  *DASHTemplate
	Segments  []Segment
}

// DASHInfo holds MPD-level static profile information.
type DASHInfo struct {
	MediaPresentationDuration core.Ticks
	MinBufferTime             core.Ticks
	Representations           []DASHRepresentation
}

func parseTemplate(dec *xml.Decoder, source string, start xml.StartElement, off int64) (*DASHTemplate, error) {
	tmpl := &DASHTemplate{StartNumber: 1, Offset: off}
	var err error
	if v := xmlAttr(start.Attr, "timescale"); v != "" {
		n, perr := strconv.ParseUint(v, 10, 32)
		if perr != nil || n == 0 {
			return nil, core.ManifestError(source, off, "invalid SegmentTemplate timescale %q", v)
		}
		tmpl.Timescale = uint32(n)
	}
	if v := xmlAttr(start.Attr, "duration"); v != "" {
		tmpl.Duration, err = strconv.ParseInt(v, 10, 64)
		if err != nil || tmpl.Duration <= 0 {
			return nil, core.ManifestError(source, off, "invalid SegmentTemplate duration %q", v)
		}
	}
	if v := xmlAttr(start.Attr, "startNumber"); v != "" {
		tmpl.StartNumber, err = strconv.ParseInt(v, 10, 64)
		if err != nil || tmpl.StartNumber < 0 {
			return nil, core.ManifestError(source, off, "invalid startNumber %q", v)
		}
	}
	tmpl.Media = xmlAttr(start.Attr, "media")
	tmpl.Initialization = xmlAttr(start.Attr, "initialization")
	if tmpl.Media == "" {
		return nil, core.ManifestError(source, off, "SegmentTemplate missing media pattern")
	}
	if err := SafeURI(source, tmpl.Media, off); err != nil {
		return nil, err
	}
	if tmpl.Initialization != "" {
		if err := SafeURI(source, tmpl.Initialization, off); err != nil {
			return nil, err
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, core.ManifestError(source, dec.InputOffset(), "truncated SegmentTemplate: %v", err)
		}
		if e, ok := tok.(xml.EndElement); ok && e.Name.Local == "SegmentTemplate" {
			return tmpl, nil
		}
		if st, ok := tok.(xml.StartElement); ok && st.Name.Local == "S" {
			sp := DASHSpan{R: -1}
			if v := xmlAttr(st.Attr, "n"); v != "" {
				sp.N, _ = strconv.ParseInt(v, 10, 64)
			}
			sp.D, err = strconv.ParseInt(xmlAttr(st.Attr, "d"), 10, 64)
			if err != nil || sp.D <= 0 {
				return nil, core.ManifestError(source, dec.InputOffset(), "invalid SegmentTimeline S duration")
			}
			if v := xmlAttr(st.Attr, "r"); v != "" {
				sp.R, err = strconv.ParseInt(v, 10, 64)
				if err != nil || sp.R < 0 {
					return nil, core.ManifestError(source, dec.InputOffset(), "invalid repeat count %q", v)
				}
			}
			tmpl.Timeline = append(tmpl.Timeline, sp)
		}
	}
}

func expandTemplate(source string, tmpl *DASHTemplate, off int64) ([]Segment, error) {
	scale := tmpl.Timescale
	if scale == 0 {
		scale = 1000
	}
	var out []Segment
	add := func(n, durUnits int64) error {
		uri := strings.ReplaceAll(tmpl.Media, "$Number$", strconv.FormatInt(n, 10))
		uri = strings.ReplaceAll(uri, "$Time$", strconv.FormatInt(n*durUnits, 10))
		if err := SafeURI(source, uri, off); err != nil {
			return err
		}
		out = append(out, Segment{Number: n, URI: uri, Offset: off,
			Duration: core.Convert(core.Ticks(durUnits), core.Timebase{Scale: scale}, core.Nanoseconds)})
		return nil
	}
	n := tmpl.StartNumber
	if len(tmpl.Timeline) > 0 {
		for _, sp := range tmpl.Timeline {
			repeats := int64(1)
			if sp.R >= 0 {
				repeats = sp.R + 1
			}
			for i := int64(0); i < repeats; i++ {
				num := n
				if sp.N != 0 {
					num = sp.N + i
				}
				if err := add(num, sp.D); err != nil {
					return nil, err
				}
				n++
			}
		}
		return out, nil
	}
	if tmpl.Duration <= 0 {
		return nil, core.ManifestError(source, off, "SegmentTemplate missing duration or SegmentTimeline")
	}
	for i := 0; i < 4; i++ {
		if err := add(n+int64(i), tmpl.Duration); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func parseListAttrs(st xml.StartElement) (uint32, int64, error) {
	var ts uint32 = 1000
	var dur int64
	var err error
	if v := xmlAttr(st.Attr, "timescale"); v != "" {
		n, e := strconv.ParseUint(v, 10, 32)
		if e != nil || n == 0 {
			return 0, 0, fmt.Errorf("invalid SegmentList timescale %q", v)
		}
		ts = uint32(n)
	}
	if v := xmlAttr(st.Attr, "duration"); v != "" {
		dur, err = strconv.ParseInt(v, 10, 64)
		if err != nil || dur <= 0 {
			return 0, 0, fmt.Errorf("invalid SegmentList duration %q", v)
		}
	}
	return ts, dur, nil
}

func parseSegmentURLs(dec *xml.Decoder, source, endName string, ts uint32, defaultDur int64) ([]Segment, error) {
	var out []Segment
	var num int64 = 1
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, core.ManifestError(source, dec.InputOffset(), "truncated SegmentList: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "SegmentURL" {
				continue
			}
			off := dec.InputOffset()
			uri := xmlAttr(t.Attr, "media")
			if uri == "" {
				return nil, core.ManifestError(source, off, "SegmentURL missing media")
			}
			if err := SafeURI(source, uri, off); err != nil {
				return nil, err
			}
			n := num
			if v := xmlAttr(t.Attr, "number"); v != "" {
				n, err = strconv.ParseInt(v, 10, 64)
				if err != nil || n < 0 {
					return nil, core.ManifestError(source, off, "invalid SegmentURL number %q", v)
				}
			}
			dur := defaultDur
			if v := xmlAttr(t.Attr, "duration"); v != "" {
				dur, err = strconv.ParseInt(v, 10, 64)
				if err != nil || dur <= 0 {
					return nil, core.ManifestError(source, off, "invalid SegmentURL duration %q", v)
				}
			}
			out = append(out, Segment{Number: n, URI: uri, Offset: off,
				Duration: core.Convert(core.Ticks(dur), core.Timebase{Scale: ts}, core.Nanoseconds)})
			num = n + 1
		case xml.EndElement:
			if t.Name.Local == endName {
				if len(out) == 0 {
					return nil, core.ManifestError(source, dec.InputOffset(), "SegmentList contains no SegmentURL")
				}
				nums := make([]int64, len(out))
				offs := make([]int64, len(out))
				for i, s := range out {
					nums[i], offs[i] = s.Number, s.Offset
				}
				if err := rejectDuplicateNumbers(source, nums, offs); err != nil {
					return nil, err
				}
				return out, nil
			}
		}
	}
}

func xmlAttr(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func parseISODuration(s string) (core.Ticks, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if !strings.HasPrefix(s, "P") {
		return 0, fmt.Errorf("not ISO duration")
	}
	s = s[1:]
	var hours, mins, secs float64
	parseUnit := func(unit string, dst *float64) error {
		i := strings.Index(s, unit)
		if i < 0 {
			return nil
		}
		n, err := strconv.ParseFloat(s[:i], 64)
		if err != nil {
			return err
		}
		*dst = n
		s = s[i+1:]
		return nil
	}
	if i := strings.Index(s, "T"); i >= 0 {
		date := s[:i]
		s = s[i+1:]
		if strings.ContainsAny(date, "YMWD") {
			return 0, fmt.Errorf("day/month durations unsupported in static fixture")
		}
		if err := parseUnit("H", &hours); err != nil {
			return 0, err
		}
		if err := parseUnit("M", &mins); err != nil {
			return 0, err
		}
		if err := parseUnit("S", &secs); err != nil {
			return 0, err
		}
	}
	d := core.Ticks((hours*3600 + mins*60 + secs) * float64(core.Scale))
	if neg {
		return -d, nil
	}
	return d, nil
}
