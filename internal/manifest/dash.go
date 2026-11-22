package manifest

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"github.com/clearliu/fieldcut/internal/clock"
)

// mpdType mirrors the small subset of ISO/IEC 23009-1 MPD we accept:
// SegmentTemplate with a SegmentTimeline, optionally an Initialization
// attribute. Everything else is ignored rather than rejected so fixture
// playlists stay compact.
type mpdType struct {
	XMLName xml.Name   `xml:"MPD"`
	Periods []periodEl `xml:"Period"`
}

type periodEl struct {
	AdaptSets []adaptSetEl `xml:"AdaptationSet"`
}

type adaptSetEl struct {
	Reps []repEl `xml:"Representation"`
}

type repEl struct {
	ID          string        `xml:"id,attr"`
	RawTemplate segTemplateEl `xml:"SegmentTemplate"`
}

type segTemplateEl struct {
	Timescale      int64             `xml:"timescale,attr"`
	Media          string            `xml:"media,attr"`
	Initialization string            `xml:"initialization,attr"`
	StartNumber    int64             `xml:"startNumber,attr"`
	Timeline       []segmentTimeline `xml:"SegmentTimeline>S"`
}

type segmentTimeline struct {
	T int64 `xml:"t,attr"` // first sample time (optional after first S)
	D int64 `xml:"d,attr"` // duration (required)
	R int64 `xml:"r,attr"` // repeat count (negative not supported)
}

// ParseDASH parses a SegmentTimeline-based MPD into SegmentEvents, one
// adaptation set / representation per track. SegmentTemplate $Number$ and
// $Time$ identifiers are expanded; only local relative URIs are allowed.
func ParseDASH(data []byte, opts Options) ([]SegmentEvent, error) {
	opts = opts.withDefaults()
	var mpd mpdType
	if err := xml.Unmarshal(bytes.TrimSpace(data), &mpd); err != nil {
		return nil, fmt.Errorf("%w: %v", clock.ErrInvalidArgument, err)
	}
	if len(mpd.Periods) == 0 {
		return nil, fmt.Errorf("%w: MPD has no Period", clock.ErrInvalidArgument)
	}
	var out []SegmentEvent
	trackID := 0
	for _, p := range mpd.Periods {
		for _, as := range p.AdaptSets {
			for ri := range as.Reps {
				if trackID >= opts.MaxTracks {
					return nil, fmt.Errorf("%w: more than %d tracks", clock.ErrManifestTooLarge, opts.MaxTracks)
				}
				evs, err := parseRep(as.Reps[ri], trackID, opts)
				if err != nil {
					return nil, err
				}
				out = append(out, evs...)
				trackID++
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: MPD produced no segments", clock.ErrInvalidArgument)
	}
	if err := CheckSequenceConflicts(out); err != nil {
		return nil, err
	}
	return out, nil
}

func parseRep(rep repEl, trackID int, opts Options) ([]SegmentEvent, error) {
	tpl := rep.RawTemplate
	scale := tpl.Timescale
	if scale <= 0 {
		scale = 1
	}
	if tpl.Media == "" {
		return nil, fmt.Errorf("%w: representation %q has no media template", clock.ErrInvalidArgument, rep.ID)
	}
	if strings.Contains(tpl.Media, "://") || strings.Contains(tpl.Initialization, "://") {
		return nil, fmt.Errorf("%w: remote template not allowed", clock.ErrPathTraversal)
	}
	start := tpl.StartNumber
	if start == 0 {
		start = 1
	}
	var events []SegmentEvent
	if tpl.Initialization != "" {
		uri, err := safePath(expandTemplate(tpl.Initialization, start, 0, rep.ID))
		if err != nil {
			return nil, err
		}
		events = append(events, SegmentEvent{
			Protocol: ProtocolDASH, Kind: KindInit, TrackID: trackID, URI: uri,
		})
	}
	var (
		num     = start
		curTime int64
		first   = true
	)
	for _, s := range tpl.Timeline {
		if s.D <= 0 {
			return nil, fmt.Errorf("%w: non-positive S@d in timeline", clock.ErrInvalidArgument)
		}
		if s.R < 0 {
			return nil, fmt.Errorf("%w: negative S@r repeat is not supported", clock.ErrInvalidArgument)
		}
		// Per 23009-1: S@t is mandatory on the first S and only needed
		// later to signal a gap in the timeline.
		if first || s.T != 0 {
			curTime = s.T
		}
		first = false
		for k := int64(0); k <= s.R; k++ {
			uri, err := safePath(expandTemplate(tpl.Media, num, curTime, rep.ID))
			if err != nil {
				return nil, err
			}
			dur := clock.Ticks(scaleTo90k(s.D, scale))
			events = append(events, SegmentEvent{
				Protocol: ProtocolDASH,
				Kind:     KindMedia,
				TrackID:  trackID,
				Sequence: int(num),
				URI:      uri,
				Start:    clock.Ticks(scaleTo90k(curTime, scale)),
				Duration: dur,
			})
			curTime += s.D
			num++
			if len(events) > opts.MaxSegments {
				return nil, fmt.Errorf("%w: > %d segments", clock.ErrManifestTooLarge, opts.MaxSegments)
			}
		}
	}
	return events, nil
}

func scaleTo90k(v, timescale int64) int64 {
	if timescale == clock.Rate {
		return v
	}
	return (v*clock.Rate + timescale/2) / timescale
}

// expandTemplate substitutes the four identifiers our fixtures use.
func expandTemplate(tpl string, number int64, time int64, repID string) string {
	r := strings.NewReplacer(
		"$Number$", strconv.FormatInt(number, 10),
		"$Time$", strconv.FormatInt(time, 10),
		"$RepresentationID$", repID,
		"$$", "$",
	)
	return r.Replace(tpl)
}
