package manifest

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"mediacore/internal/clock"
)

type mpdXML struct {
	XMLName  xml.Name `xml:"MPD"`
	Duration string   `xml:"mediaPresentationDuration,attr"`
	Periods  []struct {
		Sets []adaptationSetXML `xml:"AdaptationSet"`
	} `xml:"Period"`
}

type adaptationSetXML struct {
	ID      string `xml:"id,attr"`
	Kind    string `xml:"contentType,attr"`
	Codecs  string `xml:"codecs,attr"`
	Reps    []struct {
		ID       string             `xml:"id,attr"`
		Template segmentTemplateXML `xml:"SegmentTemplate"`
	} `xml:"Representation"`
}

type segmentTemplateXML struct {
	Timescale   int64  `xml:"timescale,attr"`
	Duration    int64  `xml:"duration,attr"`
	StartNumber int64  `xml:"startNumber,attr"`
	Media       string `xml:"media,attr"`
	Timeline    []struct {
		T int64 `xml:"t,attr"`
		D int64 `xml:"d,attr"`
		R int64 `xml:"r,attr"`
	} `xml:"SegmentTimeline>S"`
}

// ParseDASH parses an MPD into a Playlist. SegmentTemplate durations
// and SegmentTimeline entries are normalized onto the rational clock;
// negative durations, bad timescales and arithmetic overflow report
// the track and segment position.
func ParseDASH(r io.Reader) (*Playlist, error) {
	const op = "parse-dash"
	data, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, clock.Wrap(clock.CodeIO, op, err)
	}
	if len(data) > MaxManifestBytes {
		return nil, clock.At(clock.CodeManifestTooLarge, op, "", -1,
			"MPD exceeds %d bytes", MaxManifestBytes)
	}
	var doc mpdXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, clock.Wrap(clock.CodeManifestInvalid, op, err)
	}
	pl := &Playlist{Format: "dash"}
	if len(doc.Periods) == 0 {
		return nil, clock.At(clock.CodeManifestInvalid, op, "", -1, "no Period")
	}
	for _, as := range doc.Periods[0].Sets {
		if len(as.Reps) == 0 {
			continue
		}
		tpl := as.Reps[0].Template
		track := Track{TrackMeta: TrackMeta{
			ID: as.ID, Kind: as.Kind, Codec: as.Codecs, Timescale: tpl.Timescale,
		}}
		if tpl.Timescale <= 0 {
			return nil, clock.At(clock.CodeTimescaleInvalid, op, as.ID, -1,
				"timescale %d", tpl.Timescale)
		}
		var segs []Segment
		if len(tpl.Timeline) > 0 {
			segs, err = dashTimelineSegments(track.ID, tpl)
		} else if tpl.Duration > 0 {
			var total clock.Tick
			total, err = ParseISO8601Duration(doc.Duration)
			if err == nil {
				segs, err = dashFixedSegments(track.ID, tpl, total)
			}
		} else {
			err = clock.At(clock.CodeManifestInvalid, op, as.ID, -1,
				"SegmentTemplate needs duration or SegmentTimeline")
		}
		if err != nil {
			return nil, err
		}
		track.Segments = segs
		if err := ValidateTrack(track); err != nil {
			return nil, err
		}
		pl.Tracks = append(pl.Tracks, track)
	}
	return pl, nil
}

func dashTimelineSegments(trackID string, tpl segmentTemplateXML) ([]Segment, error) {
	const op = "parse-dash"
	var segs []Segment
	mediaN := tpl.StartNumber + 1
	prevEnd := clock.Zero()
	started := false
	for _, s := range tpl.Timeline {
		if s.D <= 0 {
			return nil, clock.At(clock.CodeNegativeDuration, op, trackID,
				len(segs), "S d=%d", s.D)
		}
		dur, err := clock.New(s.D, tpl.Timescale)
		if err != nil {
			return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
		}
		start := prevEnd
		if s.T != 0 || !started {
			start, err = clock.New(s.T, tpl.Timescale)
			if err != nil {
				return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
			}
		}
		if !started && start.Sign() > 0 {
			// Leading offset: keep it explicit so cross-manifest
			// alignment can be measured.
			segs = append(segs, Segment{Index: 0, Dur: start, Gap: true})
		}
		if started && start.Cmp(prevEnd) > 0 {
			// Hole between runs: keep it explicit so statistics can
			// exclude it from the effective media duration.
			hole, err := prevEnd.Sub(start)
			if err != nil {
				return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
			}
			segs = append(segs, Segment{Index: len(segs), Dur: neg(hole), Gap: true})
		}
		count := s.R + 1
		if count <= 0 || count > MaxSegments {
			return nil, clock.At(clock.CodeManifestInvalid, op, trackID,
				len(segs), "bad repeat r=%d", s.R)
		}
		for i := int64(0); i < count; i++ {
			segs = append(segs, Segment{
				Index: len(segs),
				URI:   expandMediaTemplate(tpl.Media, mediaN),
				Dur:   dur,
			})
			mediaN++
		}
		end, err := start.Add(dur)
		if err != nil {
			return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
		}
		for i := int64(1); i < count; i++ {
			end, err = end.Add(dur)
			if err != nil {
				return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
			}
		}
		prevEnd = end
		started = true
	}
	return segs, nil
}

func dashFixedSegments(trackID string, tpl segmentTemplateXML, total clock.Tick) ([]Segment, error) {
	const op = "parse-dash"
	if tpl.Duration <= 0 {
		return nil, clock.At(clock.CodeNegativeDuration, op, trackID, -1,
			"SegmentTemplate duration=%d", tpl.Duration)
	}
	dur, err := clock.New(tpl.Duration, tpl.Timescale)
	if err != nil {
		return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
	}
	var segs []Segment
	acc := clock.Zero()
	for acc.Cmp(total) < 0 {
		n := int64(len(segs)) + tpl.StartNumber + 1
		segs = append(segs, Segment{
			Index: len(segs),
			URI:   expandMediaTemplate(tpl.Media, n),
			Dur:   dur,
		})
		if len(segs) > MaxSegments {
			return nil, clock.At(clock.CodeManifestTooLarge, op, trackID, -1,
				"more than %d segments", MaxSegments)
		}
		if acc, err = acc.Add(dur); err != nil {
			return nil, clock.Wrap(clock.CodeTickOverflow, op, err)
		}
	}
	return segs, nil
}

var numberRe = regexp.MustCompile(`\$Number(%0(\d+)d)?\$`)

func expandMediaTemplate(tpl string, n int64) string {
	return numberRe.ReplaceAllStringFunc(tpl, func(m string) string {
		sub := numberRe.FindStringSubmatch(m)
		if sub[2] != "" {
			w, _ := strconv.Atoi(sub[2])
			return fmt.Sprintf("%0*d", w, n)
		}
		return strconv.FormatInt(n, 10)
	})
}

func neg(t clock.Tick) clock.Tick { return clock.Tick{Num: -t.Num, Den: t.Den} }

var isoDurRe = regexp.MustCompile(`^PT(?:(\d+)H)?(?:(\d+)M)?(?:([\d.]+)S)?$`)

// ParseISO8601Duration parses a subset of ISO 8601 durations (PTnHnMnS).
func ParseISO8601Duration(s string) (clock.Tick, error) {
	m := isoDurRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return clock.Tick{}, fmt.Errorf("bad ISO8601 duration %q", s)
	}
	total := clock.Zero()
	add := func(v string, scale int64) error {
		if v == "" {
			return nil
		}
		t, err := ParseDecimal(v)
		if err != nil {
			return err
		}
		t, err = t.MulK(scale)
		if err != nil {
			return err
		}
		total, err = total.Add(t)
		return err
	}
	if err := add(m[1], 3600); err != nil {
		return clock.Tick{}, err
	}
	if err := add(m[2], 60); err != nil {
		return clock.Tick{}, err
	}
	if err := add(m[3], 1); err != nil {
		return clock.Tick{}, err
	}
	return total, nil
}
