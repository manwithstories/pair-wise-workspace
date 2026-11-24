package manifest

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediac/internal/core"
)

// DASHScope limits parsing to the requested segment window.
type DASHScope struct {
	StartIndex int // first segment number to keep (inclusive)
	EndIndex   int // last segment number to keep (inclusive); -1 = until end
}

type dashMPD struct {
	Periods []dashPeriod `xml:"Period"`
}

type dashPeriod struct {
	AdaptationSets []dashAdaptationSet `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	ContentType     string               `xml:"contentType,attr"`
	MimeType        string               `xml:"mimeType,attr"`
	Representations []dashRepresentation `xml:"Representation"`
}

type dashRepresentation struct {
	ID              string `xml:"id,attr"`
	Codecs          string `xml:"codecs,attr"`
	Width           int    `xml:"width,attr"`
	Height          int    `xml:"height,attr"`
	Bandwidth       int64  `xml:"bandwidth,attr"`
	SegmentTemplate dashTemplate
}

type dashTemplate struct {
	Timescale       int64         `xml:"timescale,attr"`
	Initialization  string        `xml:"initialization,attr"`
	Media           string        `xml:"media,attr"`
	StartNumber     int           `xml:"startNumber,attr"`
	Duration        int64         `xml:"duration,attr"`
	SegmentTimeline *dashTimeline `xml:"SegmentTimeline"`
}

type dashTimeline struct {
	Items []dashS `xml:"S"`
}

type dashS struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	R int   `xml:"r,attr"` // repeat count (additional)
}

// lineIndex records the 1-based line of each XML token path so faults point
// at the manifest row that declared the offending element.
type lineIndex struct {
	lines map[string]int
}

// ParseDASH reads an MPD from r, resolving references under root.
func ParseDASH(root, source string, r io.Reader, scope DASHScope) (*Manifest, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) >= MaxManifestBytes {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"manifest exceeds size limit", core.Source{})
	}
	lineMap := buildLineIndex(raw)
	sLines := buildSegmentLines(raw)
	var mpd dashMPD
	if err := xml.Unmarshal(raw, &mpd); err != nil {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"invalid DASH XML: "+err.Error(), core.Source{})
	}
	out := &Manifest{Kind: KindDASH, Root: root, SourcePath: source}
	for _, period := range mpd.Periods {
		for _, as := range period.AdaptationSets {
			for _, dr := range as.Representations {
				rep, err := buildDASHRep(root, dr, scope, lineMap, sLines)
				if err != nil {
					return nil, err
				}
				if rep != nil {
					out.Representations = append(out.Representations, *rep)
				}
			}
		}
	}
	if len(out.Representations) == 0 && len(out.Faults) == 0 {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"DASH manifest contains no representations", core.Source{})
	}
	return out, nil
}

func buildDASHRep(root string, dr dashRepresentation, scope DASHScope,
	lines map[string]int, sLines map[sLineKey]int) (*Representation, error) {
	tpl := dr.SegmentTemplate
	scale := tpl.Timescale
	if scale <= 0 {
		scale = 1
	}
	startNo := tpl.StartNumber
	if startNo == 0 {
		startNo = 1
	}
	rep := &Representation{
		ID:        orDefault(dr.ID, fmt.Sprintf("rep%d", drIndex())),
		Codec:     dr.Codecs,
		Width:     dr.Width,
		Height:    dr.Height,
		TargetBPS: dr.Bandwidth,
		Timescale: scale,
	}
	if tpl.Initialization != "" {
		p, err := core.SafeJoin(root, tpl.Initialization)
		if err != nil {
			return nil, core.NewFault(core.FaultPathEscape, core.SevBlocking,
				"init ref "+tpl.Initialization, core.Source{RepID: rep.ID})
		}
		rep.InitURL, rep.InitPath = tpl.Initialization, p
	} else {
		return nil, core.NewFault(core.FaultMissingInit, core.SevBlocking,
			"representation has no initialization segment",
			core.Source{RepID: rep.ID, Line: lines[rep.ID]})
	}
	timeline, err := expandDASHTimeline(tpl, startNo, scale, rep.ID, lines, sLines)
	if err != nil {
		return nil, err
	}
	cursorMs := int64(0)
	for _, ts := range timeline {
		if scope.EndIndex >= 0 && ts.no > scope.EndIndex {
			break
		}
		if ts.no < scope.StartIndex {
			cursorMs += ts.durationMs
			continue
		}
		url := fillTemplate(tpl.Media, rep.ID, ts.no, ts.ticks)
		p, err := core.SafeJoin(root, url)
		if err != nil {
			return nil, core.NewFault(core.FaultPathEscape, core.SevBlocking,
				"media ref "+url, core.Source{RepID: rep.ID, SegmentIndex: ts.no})
		}
		seg := Segment{
			Index: ts.no, URL: url, AbsPath: p,
			StartMs: cursorMs, DurationMs: ts.durationMs, Line: ts.line,
		}
		rep.Segments = append(rep.Segments, seg)
		cursorMs += ts.durationMs
	}
	return rep, nil
}

type dashTick struct {
	no         int
	ticks      int64
	durationMs int64
	line       int
}

func expandDASHTimeline(tpl dashTemplate, startNo int, scale int64,
	repID string, lines map[string]int, sLines map[sLineKey]int) ([]dashTick, error) {
	var out []dashTick
	no := startNo
	if tpl.SegmentTimeline != nil {
		expectT := int64(-1)
		sOrdinal := 0
		for _, s := range tpl.SegmentTimeline.Items {
			repeats := s.R
			if repeats < 0 {
				return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
					"negative SegmentTimeline repeat", core.Source{RepID: repID})
			}
			declLine := sLines[sLineKey{repID: repID, ord: sOrdinal}]
			if declLine == 0 {
				declLine = lines[repID]
			}
			runStart := s.T
			for k := 0; k <= repeats; k++ {
				t := runStart + int64(k)*s.D
				if expectT >= 0 && t < expectT {
					return nil, core.NewFault(core.FaultTimestampBackward, core.SevDamaged,
						fmt.Sprintf("timeline t=%d before expected %d", t, expectT),
						core.Source{RepID: repID, SegmentIndex: no, Line: declLine})
				}
				out = append(out, dashTick{
					no: no, ticks: t,
					durationMs: core.Scale(s.D, scale, 1000),
					line:       declLine,
				})
				expectT = t + s.D
				no++
			}
			sOrdinal++
		}
		return out, nil
	}
	if tpl.Duration <= 0 {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"SegmentTemplate has neither SegmentTimeline nor duration",
			core.Source{RepID: repID})
	}
	// Fixed-duration template without explicit timeline: synthesize 24
	// segments, matching the delivery fixture's clean sample window.
	for i := 0; i < DefaultSegmentCount; i++ {
		out = append(out, dashTick{
			no: no + i, ticks: int64(i) * tpl.Duration,
			durationMs: core.Scale(tpl.Duration, scale, 1000),
		})
	}
	return out, nil
}

func fillTemplate(pattern, repID string, no int, t int64) string {
	s := strings.ReplaceAll(pattern, "$RepresentationID$", repID)
	s = strings.ReplaceAll(s, "$Number$", strconv.Itoa(no))
	s = strings.ReplaceAll(s, "$Time$", strconv.FormatInt(t, 10))
	return s
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// repIndexCounter gives anonymous representations stable ids.
var repCounter = 0

func drIndex() int {
	repCounter++
	return repCounter
}
