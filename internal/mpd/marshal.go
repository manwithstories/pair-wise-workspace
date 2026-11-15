// Package mpd serializes the shared manifest model to XML and parses it
// back, with a read-back self-check so a corrupted write is caught.
package mpd

import (
	"encoding/xml"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"dashkit/internal/model"
)

type xmlMPD struct {
	XMLName xml.Name    `xml:"MPD"`
	XMLNS   string      `xml:"xmlns,attr"`
	ID      string      `xml:"id,attr"`
	Type    string      `xml:"type,attr"`
	Dur     string      `xml:"mediaPresentationDuration,attr"`
	Periods []xmlPeriod `xml:"Period"`
}
type xmlPeriod struct {
	ID    string             `xml:"id,attr"`
	Start string             `xml:"start,attr"`
	Sets  []xmlAdaptationSet `xml:"AdaptationSet"`
}
type xmlAdaptationSet struct {
	ID          int                 `xml:"id,attr"`
	ContentType string              `xml:"contentType,attr"`
	Reps        []xmlRepresentation `xml:"Representation"`
}
type xmlRepresentation struct {
	ID        string             `xml:"id,attr"`
	Bandwidth int64              `xml:"bandwidth,attr"`
	Width     int                `xml:"width,attr,omitempty"`
	Height    int                `xml:"height,attr,omitempty"`
	FrameRate string             `xml:"frameRate,attr,omitempty"`
	Codecs    string             `xml:"codecs,attr"`
	Template  xmlSegmentTemplate `xml:"SegmentTemplate"`
}
type xmlSegmentTemplate struct {
	Timescale uint64              `xml:"timescale,attr"`
	PTO       string              `xml:"presentationTimeOffset,attr"`
	StartNum  uint64              `xml:"startNumber,attr"`
	Media     string              `xml:"media,attr"`
	Init      string              `xml:"initialization,attr"`
	Timeline  *xmlSegmentTimeline `xml:"SegmentTimeline"`
}
type xmlSegmentTimeline struct {
	S []xmlS `xml:"S"`
}
type xmlS struct {
	T *uint64 `xml:"t,attr,omitempty"`
	D uint64  `xml:"d,attr"`
	R int64   `xml:"r,attr,omitempty"`
}

// Marshal renders the model as indented XML.
func Marshal(m *model.MPD) ([]byte, error) {
	x := xmlMPD{XMLNS: "urn:mpeg:dash:schema:mpd:2011", ID: m.ID, Type: m.Type,
		Dur: formatDuration(m.MediaPresentationDuration)}
	for _, p := range m.Periods {
		xp := xmlPeriod{ID: p.ID, Start: formatDuration(p.Start)}
		for _, as := range p.Sets {
			xa := xmlAdaptationSet{ID: as.ID, ContentType: as.ContentType}
			for _, r := range as.Reps {
				xr := xmlRepresentation{ID: r.ID, Bandwidth: r.Bandwidth, Width: r.Width,
					Height: r.Height, FrameRate: r.FrameRate, Codecs: r.Codecs,
					Template: toXMLTemplate(r.Template)}
				xa.Reps = append(xa.Reps, xr)
			}
			xp.Sets = append(xp.Sets, xa)
		}
		x.Periods = append(x.Periods, xp)
	}
	out, err := xml.MarshalIndent(x, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

func toXMLTemplate(t *model.SegmentTemplate) xmlSegmentTemplate {
	xt := xmlSegmentTemplate{Timescale: t.Timescale, StartNum: t.StartNumber,
		Media: t.Media, Init: t.Initialization}
	if t.PresentationTimeOffset != nil {
		xt.PTO = t.PresentationTimeOffset.RatString()
	} else {
		xt.PTO = "0"
	}
	if t.Timeline != nil {
		xl := &xmlSegmentTimeline{}
		for _, s := range t.Timeline.S {
			xl.S = append(xl.S, xmlS{T: s.T, D: s.D, R: s.R})
		}
		xt.Timeline = xl
	}
	return xt
}

// Parse reads XML back into the shared model.
func Parse(data []byte) (*model.MPD, error) {
	var x xmlMPD
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("parse mpd: %w", err)
	}
	m := &model.MPD{ID: x.ID, Type: x.Type}
	var err error
	if m.MediaPresentationDuration, err = parseDuration(x.Dur); err != nil {
		return nil, err
	}
	for _, xp := range x.Periods {
		p := &model.Period{ID: xp.ID}
		if p.Start, err = parseDuration(xp.Start); err != nil {
			return nil, fmt.Errorf("period %s: %w", xp.ID, err)
		}
		for _, xa := range xp.Sets {
			as := &model.AdaptationSet{ID: xa.ID, ContentType: xa.ContentType}
			for _, xr := range xa.Reps {
				tpl, err := fromXMLTemplate(xr.Template)
				if err != nil {
					return nil, fmt.Errorf("rep %s: %w", xr.ID, err)
				}
				as.Reps = append(as.Reps, &model.Representation{ID: xr.ID, Bandwidth: xr.Bandwidth,
					Width: xr.Width, Height: xr.Height, FrameRate: xr.FrameRate,
					Codecs: xr.Codecs, Template: tpl})
			}
			p.Sets = append(p.Sets, as)
		}
		m.Periods = append(m.Periods, p)
	}
	return m, nil
}

func fromXMLTemplate(xt xmlSegmentTemplate) (*model.SegmentTemplate, error) {
	pto, ok := new(big.Rat).SetString(xt.PTO)
	if !ok {
		return nil, fmt.Errorf("bad presentationTimeOffset %q", xt.PTO)
	}
	t := &model.SegmentTemplate{Timescale: xt.Timescale, PresentationTimeOffset: pto,
		StartNumber: xt.StartNum, Media: xt.Media, Initialization: xt.Init}
	if xt.Timeline != nil {
		tl := &model.SegmentTimeline{}
		for _, xs := range xt.Timeline.S {
			tl.S = append(tl.S, &model.S{T: xs.T, D: xs.D, R: xs.R})
		}
		t.Timeline = tl
	}
	return t, nil
}

// WriteFile marshals, writes and reads back the manifest, returning
// read-back mismatch problems (E_READBACK_MISMATCH) when found.
func WriteFile(m *model.MPD, path string) ([]model.Problem, error) {
	data, err := Marshal(m)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	back, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Compare(m, back), nil
}

// ReadFile loads and parses an MPD document.
func ReadFile(path string) (*model.MPD, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Compare checks two models for semantic equality of what matters:
// periods, representations and expanded segment sequences.
func Compare(a, b *model.MPD) []model.Problem {
	var probs []model.Problem
	bad := func(msg string) {
		probs = append(probs, model.Problem{Code: model.CodeReadbackMismatch, Msg: msg})
	}
	if len(a.Periods) != len(b.Periods) {
		bad(fmt.Sprintf("period count %d != %d", len(a.Periods), len(b.Periods)))
		return probs
	}
	ra, rb := a.Reps(), b.Reps()
	if len(ra) != len(rb) {
		bad(fmt.Sprintf("representation count %d != %d", len(ra), len(rb)))
		return probs
	}
	for i := range ra {
		x, y := ra[i], rb[i]
		if x.ID != y.ID || x.Bandwidth != y.Bandwidth {
			bad(fmt.Sprintf("rep %d %q/%d != %q/%d", i, x.ID, x.Bandwidth, y.ID, y.Bandwidth))
			continue
		}
		sa, _ := model.ExpandTimeline(x.Template.Timeline, x.Template.StartNumber)
		sb, _ := model.ExpandTimeline(y.Template.Timeline, y.Template.StartNumber)
		if len(sa) != len(sb) {
			bad(fmt.Sprintf("rep %s segment count %d != %d", x.ID, len(sa), len(sb)))
			continue
		}
		for j := range sa {
			if sa[j] != sb[j] {
				bad(fmt.Sprintf("rep %s segment %d %+v != %+v", x.ID, j, sa[j], sb[j]))
				break
			}
		}
	}
	return probs
}

// formatDuration renders a non-negative rational of seconds as PT<x>S with
// an exact finite decimal (denominators must only contain factors 2 and 5).
func formatDuration(r *big.Rat) string {
	if r == nil {
		return "PT0S"
	}
	num, den := new(big.Int).Set(r.Num()), new(big.Int).Set(r.Denom())
	for k := 0; k <= 12; k++ {
		p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(k)), nil)
		scaled := new(big.Int).Mul(num, p)
		q, rem := new(big.Int).QuoRem(scaled, den, new(big.Int))
		if rem.Sign() == 0 {
			s := q.String()
			if k == 0 {
				return "PT" + s + "S"
			}
			for len(s) <= k {
				s = "0" + s
			}
			frac := strings.TrimRight(s[len(s)-k:], "0")
			if frac == "" {
				return "PT" + s[:len(s)-k] + "S"
			}
			return "PT" + s[:len(s)-k] + "." + frac + "S"
		}
	}
	return "PT" + r.FloatString(6) + "S"
}

// parseDuration parses PT<x>S where x is a decimal or rational.
func parseDuration(s string) (*big.Rat, error) {
	s = strings.TrimPrefix(s, "PT")
	s = strings.TrimSuffix(s, "S")
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("bad duration %q", s)
	}
	return r, nil
}
