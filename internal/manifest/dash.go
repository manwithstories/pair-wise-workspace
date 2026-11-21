package manifest

import (
	"encoding/xml"
	"fmt"

	"mediascan/internal/model"
)

// DASHTimescale is the media timescale assumed when an MPD omits one.
const DASHTimescale uint32 = 1000

// mpdDoc mirrors only the MPD subset used by SegmentTemplate timelines.
type mpdDoc struct {
	XMLName   xml.Name    `xml:"MPD"`
	Type      string      `xml:"type,attr"`
	MediaPres string      `xml:"mediaPresentationDuration,attr"`
	Periods   []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	ID       string        `xml:"id,attr"`
	Start    string        `xml:"start,attr"`
	Duration string        `xml:"duration,attr"`
	AdaptSet []mpdAdaptSet `xml:"AdaptationSet"`
}

type mpdAdaptSet struct {
	ID         string        `xml:"id,attr"`
	MimeType   string        `xml:"mimeType,attr"`
	ContentTyp string        `xml:"contentType,attr"`
	Timescale  string        `xml:"timescale,attr"`
	Templates  []mpdTemplate `xml:"SegmentTemplate"`
	Reps       []mpdRep      `xml:"Representation"`
}

type mpdRep struct {
	ID        string        `xml:"id,attr"`
	Bandwidth int64         `xml:"bandwidth,attr"`
	MimeType  string        `xml:"mimeType,attr"`
	Templates []mpdTemplate `xml:"SegmentTemplate"`
}

type mpdTemplate struct {
	Timescale      string       `xml:"timescale,attr"`
	Duration       string       `xml:"duration,attr"`
	StartNumber    string       `xml:"startNumber,attr"`
	Media          string       `xml:"media,attr"`
	Initialization string       `xml:"initialization,attr"`
	Timeline       *mpdTimeline `xml:"SegmentTimeline"`
}

type mpdTimeline struct {
	S []mpdS `xml:"S"`
}

type mpdS struct {
	T string `xml:"t,attr"`
	D string `xml:"d,attr"`
	R string `xml:"r,attr"`
}

// ParseDASH parses an MPD into the shared timeline. SegmentTemplate with
// SegmentTimeline and a constant-duration template are both supported; the
// template form expands $Number$ and $Time$ exactly.
func ParseDASH(r *Resolver, relPath string, data []byte) (*Timeline, error) {
	var doc mpdDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadManifest, err)
	}
	tl := &Timeline{Source: relPath, Format: "dash", Root: r.Root, Timescale: 1}
	if doc.MediaPres != "" {
		d, err := parseISO8601Duration(doc.MediaPres)
		if err != nil {
			return nil, err
		}
		tl.Duration = d
	}
	if len(doc.Periods) == 0 {
		return nil, fmt.Errorf("%w: no Period", ErrBadManifest)
	}
	var maxEnd model.Rational
	for pi, period := range doc.Periods {
		for ai, set := range period.AdaptSet {
			stream := Stream{
				ID:   set.ID,
				Kind: classifyKind(set.ContentTyp + " " + set.MimeType),
			}
			if stream.ID == "" {
				stream.ID = fmt.Sprintf("p%d-a%d", pi, ai)
			}
			setTemplate := firstTemplate(set.Templates)
			for _, rep := range set.Reps {
				rs := stream
				if rep.ID != "" {
					rs.ID = rep.ID
				}
				kind := classifyKind(rep.MimeType)
				if kind != "unknown" {
					rs.Kind = kind
				}
				tpl := firstTemplate(rep.Templates)
				if tpl == nil {
					tpl = setTemplate
				}
				if tpl == nil {
					return nil, fmt.Errorf("%w: representation %s has no SegmentTemplate", ErrUnsupported, rs.ID)
				}
				ts, err := parseTimescale(tpl.Timescale, set.Timescale)
				if err != nil {
					return nil, err
				}
				rs.Timescale = ts
				segs, initURI, end, err := expandTemplate(r, tpl, ts, rs.ID)
				if err != nil {
					return nil, err
				}
				rs.InitURI = initURI
				rs.Segments = segs
				rs.SegmentEnd = end
				if initURI != "" {
					tl.Referenced = append(tl.Referenced, initURI)
				}
				for _, s := range segs {
					tl.Referenced = append(tl.Referenced, s.URI)
				}
				if end.Cmp(maxEnd) > 0 {
					maxEnd = end
				}
				tl.Streams = append(tl.Streams, rs)
			}
			if len(set.Reps) == 0 && setTemplate != nil {
				// AdaptationSet-level template without Representation children.
				rs := stream
				ts, err := parseTimescale(setTemplate.Timescale, set.Timescale)
				if err != nil {
					return nil, err
				}
				rs.Timescale = ts
				segs, initURI, end, err := expandTemplate(r, setTemplate, ts, rs.ID)
				if err != nil {
					return nil, err
				}
				rs.InitURI = initURI
				rs.Segments = segs
				rs.SegmentEnd = end
				if end.Cmp(maxEnd) > 0 {
					maxEnd = end
				}
				tl.Streams = append(tl.Streams, rs)
			}
		}
	}
	if len(tl.Streams) == 0 {
		return nil, fmt.Errorf("%w: no representations", ErrBadManifest)
	}
	if tl.Duration.IsZero() {
		tl.Duration = maxEnd
	}
	// The common timeline timescale is the LCM-free maximum used for display;
	// per-segment values remain exact rationals.
	for _, s := range tl.Streams {
		if s.Timescale > tl.Timescale {
			tl.Timescale = s.Timescale
		}
	}
	return tl, nil
}
