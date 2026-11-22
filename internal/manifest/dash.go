package manifest

import (
	"encoding/xml"
	"errors"
	"fmt"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// ParseDASH parses an MPD and expands SegmentTemplate/SegmentTimeline into
// segment records in the canonical time base. Structural problems become
// faults; segments before the problem remain available.
func ParseDASH(data []byte, source string) (*Manifest, *fault.List, error) {
	if len(data) == 0 {
		return nil, nil, errors.New("manifest: empty DASH document")
	}
	var doc xmlMPD
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("manifest: parse DASH %s: %w", source, err)
	}
	canon := clock.Default()
	m := &Manifest{Kind: KindDASH}
	var faults fault.List
	addFault := func(kind fault.Kind, stream, detail string) {
		faults.Add(fault.MustNew(kind, fault.Location{Segment: source, Stream: stream, Offset: -1}, detail, true, false))
	}
	m.DASH.Profiles = doc.Profiles
	if doc.MinBufferTime != "" {
		if v, err := clock.ParseISODurationTicks(doc.MinBufferTime, canon.Timescale); err == nil {
			m.DASH.MinBufferTime, m.DASH.HasMinBufferTime = v, true
		} else {
			addFault(fault.KindManifestParse, "", "invalid minBufferTime")
		}
	}
	if doc.MediaPresentationDuration != "" {
		if v, err := clock.ParseISODurationTicks(doc.MediaPresentationDuration, canon.Timescale); err == nil {
			m.DASH.MediaPresentationDuration, m.DASH.HasMPD = v, true
		} else {
			addFault(fault.KindManifestParse, "", "invalid mediaPresentationDuration")
		}
	}
	if len(doc.Periods) == 0 {
		addFault(fault.KindManifestParse, "", "MPD has no Period")
		return m, &faults, nil
	}
	period := doc.Periods[0]
	m.DASH.PeriodID = period.ID
	periodStartCanon := int64(0)
	if period.Start != "" {
		if v, err := clock.ParseISODurationTicks(period.Start, canon.Timescale); err == nil {
			m.DASH.PeriodStart, m.DASH.HasPeriodStart = v, true
			periodStartCanon = v
		} else {
			addFault(fault.KindManifestParse, "", "invalid Period@start")
		}
	}
	// Period duration resolution: explicit Period@duration, else next Period
	// start never applies here (single Period), else MPD duration.
	periodDurCanon := int64(-1)
	if period.Duration != "" {
		if v, err := clock.ParseISODurationTicks(period.Duration, canon.Timescale); err == nil {
			m.DASH.PeriodDuration, m.DASH.HasPeriodDuration = v, true
			periodDurCanon = v
		} else {
			addFault(fault.KindManifestParse, "", "invalid Period@duration")
		}
	} else if len(doc.Periods) > 1 && doc.Periods[1].Start != "" {
		// Best effort for multi-period documents: use the next start.
		if next, err := clock.ParseISODurationTicks(doc.Periods[1].Start, canon.Timescale); err == nil {
			periodDurCanon = next - periodStartCanon
		}
	}
	if periodDurCanon < 0 && m.DASH.HasMPD {
		periodDurCanon = m.DASH.MediaPresentationDuration - periodStartCanon
	}

	for asIdx, as := range period.AdaptationSets {
		if len(as.Representations) == 0 {
			addFault(fault.KindManifestParse, fmt.Sprintf("as%d", asIdx), "AdaptationSet has no Representation")
			continue
		}
		asTemplate := mergeTemplate(nil, as.Template)
		for rIdx, xr := range as.Representations {
			repID := xr.ID
			if repID == "" {
				repID = fmt.Sprintf("as%d-r%d", asIdx, rIdx)
			}
			rep := Representation{
				ID: repID, Bandwidth: xr.Bandwidth,
				Codecs:   firstNonEmpty(xr.Codecs, as.Codecs),
				MimeType: firstNonEmpty(xr.MimeType, as.MimeType),
				Width:    firstInt(xr.Width, as.Width),
				Height:   firstInt(xr.Height, as.Height),
			}
			tmpl := asTemplate
			if xr.Template != nil {
				tmpl = mergeTemplate(as.Template, xr.Template)
			}
			if tmpl == nil || tmpl.Media == "" {
				addFault(fault.KindMissingIndex, repID, "no SegmentTemplate@media")
				m.Representations = append(m.Representations, rep)
				continue
			}
			rep.Template = tmpl
			rep.NativeTimescale = tmpl.Timescale
			rep.InitURI = expandTemplate(tmpl.Initialization, rep, 0, 0)
			segs, sf := expandRepresentation(tmpl, rep, periodDurCanon, canon)
			faults.Add(sf...)
			rep.Segments = segs
			m.Representations = append(m.Representations, rep)
		}
	}
	if m.SegmentCount() == 0 {
		addFault(fault.KindManifestParse, "", "no segments could be expanded")
	}
	return m, &faults, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
