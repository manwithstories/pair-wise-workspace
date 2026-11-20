package manifest

import (
	"encoding/xml"
	"strconv"
	"strings"

	"mediamap/internal/clock"
	"mediamap/internal/fault"
)

type sEntryXML struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int64  `xml:"r,attr"`
}

type segTemplateXML struct {
	Timescale   int64  `xml:"timescale,attr"`
	Duration    int64  `xml:"duration,attr"`
	StartNumber int64  `xml:"startNumber,attr"`
	Media       string `xml:"media,attr"`
	Timeline    *struct {
		S []sEntryXML `xml:"S"`
	} `xml:"SegmentTimeline"`
}

type representationXML struct {
	ID        string          `xml:"id,attr"`
	Bandwidth int             `xml:"bandwidth,attr"`
	Template  *segTemplateXML `xml:"SegmentTemplate"`
}

type mpdXML struct {
	XMLName  xml.Name `xml:"MPD"`
	Duration string   `xml:"mediaPresentationDuration,attr"`
	Periods  []struct {
		Sets []struct {
			ContentType string              `xml:"contentType,attr"`
			Reps        []representationXML `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

// ParseDASH parses an MPD using SegmentTemplate (with SegmentTimeline, or
// with a fixed duration plus mediaPresentationDuration) into a track per
// adaptation set. Missing indexes fall back to derived boundaries that are
// flagged as such, never to silent guesses.
func ParseDASH(data []byte) ([]Track, fault.List) {
	faults := fault.List{}
	var doc mpdXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fault.List{fault.New(fault.StageManifest, "dash", fault.NoOffset, false,
			"MPD is not well-formed XML: %v", err)}
	}
	var total clock.Rational
	hasTotal := false
	if doc.Duration != "" {
		if r, err := parsePTDuration(doc.Duration); err == nil {
			total, hasTotal = r, true
		}
	}
	var tracks []Track
	for _, p := range doc.Periods {
		for si, set := range p.Sets {
			tr := Track{
				Name:       "set-" + strconv.Itoa(si),
				Kind:       set.ContentType,
				Confidence: Indexed,
			}
			for _, rx := range set.Reps {
				if rx.Template == nil {
					faults = append(faults, fault.New(fault.StageManifest, tr.Name, fault.NoOffset, false,
						"representation %q has no SegmentTemplate", rx.ID))
					continue
				}
				if tr.Timescale == 0 {
					tr.Timescale = rx.Template.Timescale
				}
				segs, fs := expandTemplate(tr.Name, rx, total, hasTotal)
				faults = append(faults, fs...)
				rep := Representation{ID: rx.ID, Bandwidth: rx.Bandwidth, Segments: segs}
				for _, s := range segs {
					if s.Derived {
						tr.Confidence = Derived
					}
				}
				tr.Representations = append(tr.Representations, rep)
			}
			tracks = append(tracks, tr)
		}
	}
	if len(tracks) == 0 {
		faults = append(faults, fault.New(fault.StageManifest, "dash", fault.NoOffset, false,
			"MPD contains no adaptation set"))
	}
	return tracks, faults
}

// expandTemplate expands a SegmentTemplate into concrete segment refs on
// the 90 kHz timeline. Timeline entries win; otherwise a fixed duration
// plus the presentation duration yields derived boundaries.
func expandTemplate(track string, rx representationXML, total clock.Rational, hasTotal bool) ([]SegmentRef, fault.List) {
	faults := fault.List{}
	tpl := rx.Template
	if tpl.Timescale <= 0 {
		return nil, fault.List{fault.New(fault.StageManifest, track, fault.NoOffset, false,
			"representation %q has non-positive timescale", rx.ID)}
	}
	var segs []SegmentRef
	num := tpl.StartNumber
	add := func(startTicks, durTicks int64, derived bool) {
		uri := strings.ReplaceAll(tpl.Media, "$RepresentationID$", rx.ID)
		uri = strings.ReplaceAll(uri, "$Number$", strconv.FormatInt(num, 10))
		segs = append(segs, SegmentRef{URI: uri, Start: startTicks, Duration: durTicks, Derived: derived})
		num++
	}
	if tpl.Timeline != nil && len(tpl.Timeline.S) > 0 {
		var cursor int64 // source-timescale ticks
		for _, e := range tpl.Timeline.S {
			if e.D <= 0 {
				faults = append(faults, fault.New(fault.StageManifest, track, fault.NoOffset, true,
					"timeline entry with non-positive duration skipped"))
				continue
			}
			if e.T != nil {
				cursor = *e.T
			}
			for i := int64(0); i < e.R+1; i++ {
				add(clock.ToTimebase(cursor, tpl.Timescale).Ticks(),
					clock.ToTimebase(e.D, tpl.Timescale).Ticks(), false)
				cursor += e.D
			}
		}
		return segs, faults
	}
	if tpl.Duration > 0 && hasTotal {
		dur := clock.ToTimebase(tpl.Duration, tpl.Timescale)
		n := clock.NewRational(total.Num*tpl.Timescale, total.Den*tpl.Duration).Ticks()
		for i := int64(0); i < n; i++ {
			start := clock.NewRational(dur.Num*i, dur.Den)
			add(start.Ticks(), dur.Ticks(), true)
		}
		faults = append(faults, fault.New(fault.StageManifest, track, fault.NoOffset, true,
			"representation %q: no SegmentTimeline; %d boundaries derived from duration", rx.ID, n))
		return segs, faults
	}
	faults = append(faults, fault.New(fault.StageManifest, track, fault.NoOffset, false,
		"representation %q: no SegmentTimeline and no derivable duration", rx.ID))
	return nil, faults
}

// parsePTDuration parses the subset of ISO 8601 used by MPDs here: PT<n>S
// with an optional fractional part.
func parsePTDuration(s string) (clock.Rational, error) {
	s = strings.TrimPrefix(s, "PT")
	s = strings.TrimSuffix(s, "S")
	return parseDecimal(s)
}
