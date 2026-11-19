package manifest

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"archive/internal/errs"
)

// DASH MPD structures: SegmentTemplate (with SegmentTimeline) and
// SegmentList are supported, at AdaptationSet or Representation level.
type mpd struct {
	Periods []period `xml:"Period"`
}
type period struct {
	Sets []adaptationSet `xml:"AdaptationSet"`
}
type adaptationSet struct {
	SegmentTemplate *segTemplate `xml:"SegmentTemplate"`
	SegmentList     *segList     `xml:"SegmentList"`
	Reps            []rep        `xml:"Representation"`
}
type rep struct {
	SegmentTemplate *segTemplate `xml:"SegmentTemplate"`
	SegmentList     *segList     `xml:"SegmentList"`
}
type segTemplate struct {
	Media       string    `xml:"media,attr"`
	Timescale   uint32    `xml:"timescale,attr"`
	Duration    uint32    `xml:"duration,attr"`
	StartNumber int       `xml:"startNumber,attr"`
	Timeline    *timeline `xml:"SegmentTimeline"`
}
type timeline struct {
	S []sElem `xml:"S"`
}
type sElem struct {
	T uint64 `xml:"t,attr"`
	D uint64 `xml:"d,attr"`
	R int    `xml:"r,attr"`
}
type segList struct {
	Timescale   uint32    `xml:"timescale,attr"`
	Duration    uint32    `xml:"duration,attr"`
	StartNumber int       `xml:"startNumber,attr"`
	Timeline    *timeline `xml:"SegmentTimeline"`
	URLs        []segURL  `xml:"SegmentURL"`
}
type segURL struct {
	Media string `xml:"media,attr"`
}

func parseDASH(asset, text string) (*Manifest, error) {
	var doc mpd
	if err := xml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, errs.New(errs.KindManifest, asset, "MPD", "malformed XML", err)
	}
	m := &Manifest{Type: TypeDASH}
	var timescale uint32
	index := 0
	for pi, p := range doc.Periods {
		for ai, as := range p.Sets {
			tpl, list := as.SegmentTemplate, as.SegmentList
			if len(as.Reps) > 0 {
				if as.Reps[0].SegmentTemplate != nil {
					tpl = as.Reps[0].SegmentTemplate
				}
				if as.Reps[0].SegmentList != nil {
					list = as.Reps[0].SegmentList
				}
			}
			loc := fmt.Sprintf("period %d adaptationSet %d", pi, ai)
			var refs []SegmentRef
			var ts uint32
			var err error
			switch {
			case tpl != nil:
				refs, ts, err = expandTemplate(asset, loc, tpl, index)
			case list != nil:
				refs, ts, err = expandList(asset, loc, list, index)
			default:
				return nil, errs.New(errs.KindManifest, asset, loc, "no SegmentTemplate or SegmentList", nil)
			}
			if err != nil {
				return nil, err
			}
			if ts == 0 {
				return nil, errs.New(errs.KindManifest, asset, loc, "missing timescale", nil)
			}
			if timescale == 0 {
				timescale = ts
			} else if timescale != ts {
				return nil, errs.Newf(errs.KindManifest, asset, loc,
					"inconsistent timescale %d, expected %d", ts, timescale)
			}
			m.Segments = append(m.Segments, refs...)
			index += len(refs)
		}
	}
	if len(m.Segments) == 0 {
		return nil, errs.New(errs.KindManifest, asset, "MPD", "no segments found", nil)
	}
	m.Timescale = timescale
	return m, nil
}

func expandTemplate(asset, loc string, t *segTemplate, base int) ([]SegmentRef, uint32, error) {
	if t.Media == "" {
		return nil, 0, errs.New(errs.KindManifest, asset, loc, "SegmentTemplate missing media pattern", nil)
	}
	if t.Timeline == nil || len(t.Timeline.S) == 0 {
		return nil, 0, errs.New(errs.KindManifest, asset, loc,
			"SegmentTemplate without SegmentTimeline is not resolvable", nil)
	}
	start := t.StartNumber
	if start == 0 {
		start = 1
	}
	var refs []SegmentRef
	num := start
	for _, s := range t.Timeline.S {
		if s.D == 0 {
			return nil, 0, errs.New(errs.KindManifest, asset, loc, "zero segment duration in timeline", nil)
		}
		if s.R < 0 {
			return nil, 0, errs.New(errs.KindManifest, asset, loc, "open-ended repeat (r=-1) unsupported", nil)
		}
		for i := 0; i <= s.R; i++ {
			uri := numberPattern(t.Media, num)
			if err := ValidateURI(asset, uri); err != nil {
				return nil, 0, err
			}
			refs = append(refs, SegmentRef{
				Index: base + len(refs), URI: uri,
				Duration: float64(s.D) / float64(t.Timescale), Timescale: t.Timescale,
			})
			num++
		}
	}
	return refs, t.Timescale, nil
}

func expandList(asset, loc string, l *segList, base int) ([]SegmentRef, uint32, error) {
	if len(l.URLs) == 0 {
		return nil, 0, errs.New(errs.KindManifest, asset, loc, "SegmentList has no SegmentURL", nil)
	}
	var dur float64
	if l.Duration > 0 && l.Timescale > 0 {
		dur = float64(l.Duration) / float64(l.Timescale)
	}
	refs := make([]SegmentRef, 0, len(l.URLs))
	for i, u := range l.URLs {
		d := dur
		if l.Timeline != nil && i < len(l.Timeline.S) && l.Timescale > 0 {
			d = float64(l.Timeline.S[i].D) / float64(l.Timescale)
		}
		if d <= 0 {
			return nil, 0, errs.Newf(errs.KindManifest, asset, loc, "no positive duration for segment %d", i)
		}
		if err := ValidateURI(asset, u.Media); err != nil {
			return nil, 0, err
		}
		refs = append(refs, SegmentRef{Index: base + i, URI: u.Media, Duration: d, Timescale: l.Timescale})
	}
	return refs, l.Timescale, nil
}

// numberPattern expands $Number$ and $Number%0Nd$ placeholders.
func numberPattern(pattern string, n int) string {
	s := strings.ReplaceAll(pattern, "$$", "\x00")
	if i := strings.Index(s, "$Number"); i >= 0 {
		end := strings.Index(s[i+1:], "$")
		if end >= 0 {
			tag := s[i : i+end+2]
			repl := strconv.Itoa(n)
			if j := strings.Index(tag, "%0"); j >= 0 && strings.HasSuffix(tag, "d$") {
				if w, err := strconv.Atoi(tag[j+2 : len(tag)-2]); err == nil {
					repl = fmt.Sprintf("%0*d", w, n)
				}
			}
			s = strings.Replace(s, tag, repl, 1)
		}
	}
	return strings.ReplaceAll(s, "\x00", "$")
}
