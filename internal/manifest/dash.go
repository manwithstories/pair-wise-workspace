package manifest

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"archive/internal/clock"
	"archive/internal/fault"
)

type dashS struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int64  `xml:"r,attr"`
}

type dashTemplate struct {
	Timescale   int64  `xml:"timescale,attr"`
	Duration    int64  `xml:"duration,attr"`
	Media       string `xml:"media,attr"`
	StartNumber int64  `xml:"startNumber,attr"`
	Timeline    struct {
		S []dashS `xml:"S"`
	} `xml:"SegmentTimeline"`
}

type dashMPD struct {
	XMLName xml.Name `xml:"MPD"`
	Periods []struct {
		Sets []struct {
			Templates []dashTemplate `xml:"SegmentTemplate"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

var tplVar = regexp.MustCompile(`\$(Number|Time)(%0(\d+)d)?\$`)

// ParseDASH parses an MPD using SegmentTemplate with either an
// explicit SegmentTimeline or a fixed duration plus SegmentTimeline
// repeat arithmetic. Negative fields and overflowing timelines are
// rejected; media templates must not escape the manifest base.
func ParseDASH(data []byte, source, base string) ([]Segment, error) {
	var mpd dashMPD
	if err := xml.Unmarshal(data, &mpd); err != nil {
		return nil, fault.Wrap(fault.Fatal, "dash", source, err)
	}
	var tmpl *dashTemplate
	for i := range mpd.Periods {
		for j := range mpd.Periods[i].Sets {
			for k := range mpd.Periods[i].Sets[j].Templates {
				if tmpl != nil {
					return nil, fault.Newf(fault.Fatal, "dash", source, "multiple SegmentTemplates not supported")
				}
				tmpl = &mpd.Periods[i].Sets[j].Templates[k]
			}
		}
	}
	if tmpl == nil {
		return nil, fault.Newf(fault.Fatal, "dash", source, "no SegmentTemplate found")
	}
	if tmpl.Timescale <= 0 {
		return nil, fault.Newf(fault.Fatal, "dash", source, "timescale must be positive")
	}
	if tmpl.StartNumber < 0 {
		return nil, fault.Newf(fault.Fatal, "dash", source, "negative startNumber")
	}
	if strings.Contains(tmpl.Media, "$$") || tplVar.FindString(tmpl.Media) == "" && strings.Contains(tmpl.Media, "$") {
		return nil, fault.Newf(fault.Fatal, "dash", source, "unsupported template variable in media")
	}
	if len(tmpl.Timeline.S) == 0 {
		return nil, fault.Newf(fault.Fatal, "dash", source, "SegmentTimeline required")
	}
	return expandTimeline(tmpl, source, base)
}

func expandTimeline(t *dashTemplate, source, base string) ([]Segment, error) {
	var segs []Segment
	var nextT int64
	for si, s := range t.Timeline.S {
		if s.D <= 0 {
			return nil, fault.Newf(fault.Fatal, "dash", source, "S[%d]: non-positive duration %d", si, s.D)
		}
		if s.R < -1 {
			return nil, fault.Newf(fault.Fatal, "dash", source, "S[%d]: invalid repeat %d", si, s.R)
		}
		start := nextT
		if s.T != nil {
			if *s.T < 0 {
				return nil, fault.Newf(fault.Fatal, "dash", source, "S[%d]: negative t", si)
			}
			start = *s.T
		} else if si == 0 {
			start = 0
		}
		reps := s.R + 1
		if s.R == -1 {
			return nil, fault.Newf(fault.Fatal, "dash", source, "S[%d]: unbounded repeat not supported", si)
		}
		for r := int64(0); r < reps; r++ {
			// overflow-checked: cur = start + r*d
			cur, ok := checkedAddMul(start, r, s.D)
			if !ok {
				return nil, fault.Newf(fault.Fatal, "dash", source, "S[%d]: timeline overflow", si)
			}
			dur, err := clock.New(s.D, t.Timescale)
			if err != nil {
				return nil, fault.Wrap(fault.Fatal, "dash", source, err)
			}
			st, err := clock.New(cur, t.Timescale)
			if err != nil {
				return nil, fault.Wrap(fault.Fatal, "dash", source, err)
			}
			num := t.StartNumber + int64(len(segs))
			uri, err := SafeJoin(base, expandTemplate(t.Media, num, cur))
			if err != nil {
				return nil, err
			}
			segs = append(segs, Segment{
				Index: len(segs), URI: uri, Start: st, Dur: dur,
				Anchor: clock.Must(0, 1), Source: source,
			})
			nextT = cur + s.D
		}
	}
	if len(segs) == 0 {
		return nil, fault.Newf(fault.Fatal, "dash", source, "timeline expands to zero segments")
	}
	return segs, nil
}

func checkedAddMul(base, mul, step int64) (int64, bool) {
	if mul != 0 && step > (1<<62-1)/mul {
		return 0, false
	}
	v := base + mul*step
	if v < 0 || v > 1<<62 {
		return 0, false
	}
	return v, true
}

// expandTemplate substitutes $Number$ / $Time$ (with optional %0Nd).
func expandTemplate(tpl string, number, t int64) string {
	return tplVar.ReplaceAllStringFunc(tpl, func(m string) string {
		sub := tplVar.FindStringSubmatch(m)
		var v int64
		if sub[1] == "Number" {
			v = number
		} else {
			v = t
		}
		if sub[3] != "" {
			w, _ := strconv.Atoi(sub[3])
			return fmt.Sprintf("%0*d", w, v)
		}
		return strconv.FormatInt(v, 10)
	})
}
