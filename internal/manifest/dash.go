package manifest

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ParseDASHFile parses a local MPD document.
func ParseDASHFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: open dash %s: %w", path, err)
	}
	defer f.Close()
	m, err := ParseDASH(f)
	if err != nil {
		return nil, err
	}
	m.SourcePath = path
	return m, nil
}

type dashMPD struct {
	XMLName xml.Name     `xml:"MPD"`
	Periods []dashPeriod `xml:"Period"`
}

type dashPeriod struct {
	AdaptationSets []dashAS `xml:"AdaptationSet"`
}

type dashAS struct {
	ID              string        `xml:"id,attr"`
	ContentType     string        `xml:"contentType,attr"`
	MimeType        string        `xml:"mimeType,attr"`
	Lang            string        `xml:"lang,attr"`
	SamplingRate    string        `xml:"audioSamplingRate,attr"`
	ChannelConfig   []dashChanCfg `xml:"AudioChannelConfiguration"`
	Representations []dashRepr    `xml:"Representation"`
	Template        *dashTemplate `xml:"SegmentTemplate"`
}

type dashChanCfg struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

type dashRepr struct {
	ID            string        `xml:"id,attr"`
	Codecs        string        `xml:"codecs,attr"`
	Bandwidth     int           `xml:"bandwidth,attr"`
	SamplingRate  string        `xml:"audioSamplingRate,attr"`
	ChannelConfig []dashChanCfg `xml:"AudioChannelConfiguration"`
	Template      *dashTemplate `xml:"SegmentTemplate"`
}

type dashTemplate struct {
	Timescale              uint32        `xml:"timescale,attr"`
	Media                  string        `xml:"media,attr"`
	Initialization         string        `xml:"initialization,attr"`
	StartNumber            int64         `xml:"startNumber,attr"`
	PresentationTimeOffset int64         `xml:"presentationTimeOffset,attr"`
	Timeline               *dashTimeline `xml:"SegmentTimeline"`
}

type dashTimeline struct {
	S []dashS `xml:"S"`
}

type dashS struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int    `xml:"r,attr"`
}

// ParseDASH parses an MPD from a reader and expands SegmentTimeline
// repeat counts into the flat Segment list.
func ParseDASH(r io.Reader) (*Manifest, error) {
	var doc dashMPD
	dec := xml.NewDecoder(r)
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("manifest: decode mpd: %w", err)
	}

	m := &Manifest{Kind: KindDASH}
	for _, p := range doc.Periods {
		for _, as := range p.AdaptationSets {
			asTpl := as.Template
			for _, rep := range as.Representations {
				tpl := rep.Template
				if tpl == nil {
					tpl = asTpl
				}
				if tpl == nil {
					return nil, fmt.Errorf("manifest: representation %s has no SegmentTemplate", rep.ID)
				}
				if err := CheckRef(tpl.Media); err != nil {
					return nil, fmt.Errorf("manifest: representation %s: %w", rep.ID, err)
				}
				if tpl.Initialization != "" {
					if err := CheckRef(tpl.Initialization); err != nil {
						return nil, err
					}
				}
				ts := tpl.Timescale
				if ts == 0 {
					ts = 1000 // DASH default timescale is milliseconds
				}
				kind := classifyTrack(as.ContentType, as.MimeType)
				rate := firstNonEmpty(rep.SamplingRate, as.SamplingRate)
				track := TrackDesc{
					ID:         firstNonEmpty(rep.ID, as.ID),
					Kind:       kind,
					Codec:      rep.Codecs,
					Timescale:  ts,
					SampleRate: parseUint32(rate),
				}
				if cc := firstChannelConfig(rep.ChannelConfig, as.ChannelConfig); cc != "" {
					track.Channels = int(parseUint32(cc))
				}
				m.Tracks = append(m.Tracks, track)
				if tpl.PresentationTimeOffset != 0 && m.PresentationTimeOffset == 0 {
					m.PresentationTimeOffset = tpl.PresentationTimeOffset
				}
				m.Anchors = append(m.Anchors, Anchor{
					Kind: AnchorPTO, TrackID: track.ID, SegmentIndex: 0,
					Ticks: tpl.PresentationTimeOffset,
				})

				startNumber := tpl.StartNumber
				if startNumber == 0 {
					startNumber = 1
				}
				if tpl.Timeline == nil {
					return nil, fmt.Errorf("manifest: representation %s missing SegmentTimeline", rep.ID)
				}
				var cursor int64
				repCount := 0
				for _, s := range tpl.Timeline.S {
					if s.T != nil {
						cursor = *s.T
					}
					repeat := s.R + 1 // r counts extra repetitions
					for k := 0; k < repeat; k++ {
						n := startNumber + int64(repCount)
						uri := expandTemplate(tpl.Media, rep.ID, n, cursor)
						if err := CheckRef(uri); err != nil {
							return nil, err
						}
						init := expandTemplate(tpl.Initialization, rep.ID, n, cursor)
						seg := Segment{
							TrackID:       track.ID,
							Index:         len(m.Segments),
							Sequence:      n,
							URI:           uri,
							MapURI:        init,
							DurationTicks: s.D,
							StartTicks:    cursor,
						}
						seg.DurationSec = float64(s.D) / float64(ts)
						m.Segments = append(m.Segments, seg)
						cursor += s.D
						repCount++
					}
				}
			}
		}
	}
	if len(m.Segments) == 0 {
		return nil, fmt.Errorf("manifest: mpd contains no timeline segments")
	}
	return m, nil
}

func classifyTrack(contentType, mimeType string) TrackKind {
	s := strings.ToLower(contentType + " " + mimeType)
	switch {
	case strings.Contains(s, "video"):
		return TrackVideo
	case strings.Contains(s, "audio"):
		return TrackAudio
	default:
		return TrackData
	}
}

// expandTemplate performs the small subset of DASH template expansion
// used by the fixtures: $Number$, width-prefixed $Number%0Nd$, $Time$
// and $RepresentationID$.
func expandTemplate(tmpl, repID string, number, time int64) string {
	if tmpl == "" {
		return ""
	}
	out := tmpl
	out = strings.ReplaceAll(out, "$RepresentationID$", repID)
	out = strings.ReplaceAll(out, "$Time$", strconv.FormatInt(time, 10))
	for {
		i := strings.Index(out, "$Number%")
		if i < 0 {
			break
		}
		j := strings.Index(out[i:], "d$")
		if j < 0 {
			break
		}
		width, err := strconv.Atoi(out[i+len("$Number%") : i+j])
		if err != nil {
			break
		}
		token := out[i : i+j+2]
		out = strings.Replace(out, token, fmt.Sprintf("%0*d", width, number), 1)
	}
	out = strings.ReplaceAll(out, "$Number$", strconv.FormatInt(number, 10))
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstChannelConfig(groups ...[]dashChanCfg) string {
	for _, g := range groups {
		if len(g) > 0 && g[0].Value != "" {
			return g[0].Value
		}
	}
	return ""
}

func parseUint32(s string) uint32 {
	s = strings.TrimSpace(s)
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}
