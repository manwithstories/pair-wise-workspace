package manifest

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// dashMPD is the minimal MPD tree the fixture and the parser exchange. It
// supports SegmentTemplate (timed media) with explicit SegmentTimeline.
type dashMPD struct {
	XMLName         xml.Name           `xml:"MPD"`
	MediaPresent    string             `xml:"mediaPresentationDuration,attr"`
	MinBuffer       string             `xml:"minimumUpdatePeriod,attr"`
	Periods         []dashPeriod       `xml:"Period"`
}

type dashPeriod struct {
	ID             string                `xml:"id,attr"`
	AdaptationSets []dashAdaptationSet   `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	ContentType     string              `xml:"contentType,attr"`
	MimeType        string              `xml:"mimeType,attr"`
	SegmentTemplate *dashSegmentTemplate `xml:"SegmentTemplate"`
	Representations []dashRepresentation `xml:"Representation"`
}

type dashSegmentTemplate struct {
	Timescale      int64          `xml:"timescale,attr"`
	Initialization  string         `xml:"initialization,attr"`
	Media           string         `xml:"media,attr"`
	StartNumber     int            `xml:"startNumber,attr"`
	Duration        int64          `xml:"duration,attr"`
	Timeline        []dashTimelineEntry `xml:"SegmentTimeline>S"`
}

type dashTimelineEntry struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	R int   `xml:"r,attr"`
}

type dashRepresentation struct {
	ID         string `xml:"id,attr"`
	Codecs     string `xml:"codecs,attr"`
	Bandwidth  int    `xml:"bandwidth,attr"`
	Width      int    `xml:"width,attr"`
	Height     int    `xml:"height,attr"`
	AudioSamplingRate string `xml:"audioSamplingRate,attr"`
	NumChannels int   `xml:"audioChannelConfiguration,attr"`
}

// ParseDASH reads an MPD at mpdPath and expands every representation's
// segment list from its SegmentTemplate/SegmentTimeline.
func ParseDASH(camera, mpdPath string) (*Manifest, error) {
	baseDir := filepath.Dir(mpdPath)
	data, err := os.ReadFile(mpdPath)
	if err != nil {
		return nil, fmt.Errorf("read mpd %s: %w", mpdPath, err)
	}
	var mpd dashMPD
	if err := xml.Unmarshal(data, &mpd); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if len(mpd.Periods) == 0 {
		return nil, fmt.Errorf("%w: no Period", ErrInvalidManifest)
	}

	m := &Manifest{
		Format:    FormatDASH,
		Camera:    camera,
		BaseDir:   baseDir,
		MasterURI: filepath.Base(mpdPath),
	}
	for _, period := range mpd.Periods {
		for si, as := range period.AdaptationSets {
			tpl := as.SegmentTemplate
			if tpl == nil {
				return nil, fmt.Errorf("%w: AdaptationSet %d missing SegmentTemplate", ErrInvalidManifest, si)
			}
			role := roleFromContent(as.ContentType, si)
			for _, dr := range as.Representations {
				rep := Representation{
					ID:         camera + "/" + dr.ID,
					Role:       role,
					Codec:      dr.Codecs,
					Bandwidth:  dr.Bandwidth,
					Width:      dr.Width,
					Height:     dr.Height,
					SampleRate: atoiOr(dr.AudioSamplingRate, 0),
					Channels:   dr.NumChannels,
				}
				segs, err := expandTemplate(baseDir, tpl, dr.ID)
				if err != nil {
					return nil, err
				}
				rep.Segments = segs
				m.Representations = append(m.Representations, rep)
			}
		}
	}
	if len(m.Representations) == 0 {
		return nil, fmt.Errorf("%w: no representations", ErrInvalidManifest)
	}
	return m, nil
}

func expandTemplate(baseDir string, tpl *dashSegmentTemplate, repID string) ([]Segment, error) {
	timescale := tpl.Timescale
	if timescale <= 0 {
		timescale = 1
	}
	start := tpl.StartNumber
	if start == 0 {
		start = 1
	}
	var segs []Segment
	seq := start
	var timeline []dashTimelineEntry
	if len(tpl.Timeline) > 0 {
		timeline = tpl.Timeline
	} else {
		// No explicit timeline: not used by the fixture, but keep a sane error.
		return nil, fmt.Errorf("%w: SegmentTimeline required", ErrInvalidManifest)
	}
	var timeCursor int64
	for _, e := range timeline {
		if e.T > 0 {
			timeCursor = e.T
		}
		repeat := e.R
		if repeat < 0 {
			repeat = 0
		}
		for k := 0; k <= repeat; k++ {
			uri := fillTemplate(tpl.Media, repID, seq, timeCursor)
			if _, err := safeJoin(baseDir, uri); err != nil {
				return nil, fmt.Errorf("segment %q: %w", uri, err)
			}
			dur90k := scaleTo90k(e.D, timescale)
			segs = append(segs, Segment{
				Sequence:    seq,
				URI:         uri,
				Start90k:    scaleTo90k(timeCursor, timescale),
				Duration90k: dur90k,
			})
			timeCursor += e.D
			seq++
		}
	}
	if idx, err := checkSequence(segs); err != nil {
		return nil, fmt.Errorf("%w at position %d", err, idx)
	}
	return segs, nil
}

func scaleTo90k(value, timescale int64) int64 {
	if timescale <= 0 {
		return value
	}
	// value / timescale seconds -> ticks.
	return (value*90000 + timescale/2) / timescale
}

func fillTemplate(tmpl, repID string, number int, time int64) string {
	out := tmpl
	out = strings.ReplaceAll(out, "$RepresentationID$", repID)
	out = strings.ReplaceAll(out, "$Number$", strconv.Itoa(number))
	out = strings.ReplaceAll(out, "$Time$", strconv.FormatInt(time, 10))
	return out
}

func roleFromContent(contentType string, idx int) string {
	// The fixture encodes the role into the contentType ("video/primary"...).
	if strings.Contains(contentType, "backup") {
		return "backup"
	}
	if strings.Contains(contentType, "primary") {
		return "primary"
	}
	if idx == 0 {
		return "primary"
	}
	return "backup"
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
