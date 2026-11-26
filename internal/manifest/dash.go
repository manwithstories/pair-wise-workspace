package manifest

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"aircheck/internal/media"
)

// MPD is the subset of a DASH manifest aircheck consumes. Attribute names are
// matched by the standard xmlns namespace; unknown namespaces are ignored by
// encoding/xml, so a plain local MPD parses.
type MPD struct {
	XMLName                   xml.Name `xml:"MPD"`
	Type                      string   `xml:"type,attr"`
	Profiles                  string   `xml:"profiles,attr"`
	MediaPresentationDuration string   `xml:"mediaPresentationDuration,attr"`
	MinBufferTime             string   `xml:"minBufferTime,attr"`
	BaseURL                   string   `xml:"BaseURL"`
	Periods                   []Period `xml:"Period"`
}

// Period is a contiguous presentation interval.
type Period struct {
	ID             string          `xml:"id,attr"`
	Start          string          `xml:"start,attr"`
	Duration       string          `xml:"duration,attr"`
	BaseURL        string          `xml:"BaseURL"`
	AdaptationSets []AdaptationSet `xml:"AdaptationSet"`
}

// AdaptationSet groups interchangeable representations.
type AdaptationSet struct {
	ID              string           `xml:"id,attr"`
	MimeType        string           `xml:"mimeType,attr"`
	Codecs          string           `xml:"codecs,attr"`
	Lang            string           `xml:"lang,attr"`
	BaseURL         string           `xml:"BaseURL"`
	ContentType     string           `xml:"contentType,attr"`
	SegmentTemplate *SegmentTemplate `xml:"SegmentTemplate"`
	Representations []Representation `xml:"Representation"`
}

// Representation is one bitrate/track variant.
type Representation struct {
	ID                string           `xml:"id,attr"`
	Bandwidth         string           `xml:"bandwidth,attr"`
	Codecs            string           `xml:"codecs,attr"`
	MimeType          string           `xml:"mimeType,attr"`
	Width             string           `xml:"width,attr"`
	Height            string           `xml:"height,attr"`
	FrameRate         string           `xml:"frameRate,attr"`
	AudioSamplingRate string           `xml:"audioSamplingRate,attr"`
	BaseURL           string           `xml:"BaseURL"`
	SegmentTemplate   *SegmentTemplate `xml:"SegmentTemplate"`
}

// SegmentTemplate describes numbered segments and their init section.
type SegmentTemplate struct {
	Media                  string           `xml:"media,attr"`
	Initialization         string           `xml:"initialization,attr"`
	StartNumber            string           `xml:"startNumber,attr"`
	Timescale              string           `xml:"timescale,attr"`
	Duration               string           `xml:"duration,attr"`
	PresentationTimeOffset string           `xml:"presentationTimeOffset,attr"`
	SegmentTimeline        *SegmentTimeline `xml:"SegmentTimeline"`
}

// SegmentTimeline is the explicit timing list.
type SegmentTimeline struct {
	Entries []SegmentEntry `xml:"S"`
}

// SegmentEntry is one S element.
type SegmentEntry struct {
	T string `xml:"t,attr"`
	D string `xml:"d,attr"`
	R string `xml:"r,attr"`
	N string `xml:"n,attr"`
}

// DashTrack is a resolved representation: its timeline mapped into clock ticks
// plus concrete segment references.
type DashTrack struct {
	Kind       media.Kind
	RepID      string
	Codecs     string
	Bandwidth  int64
	MimeType   string
	FrameRate  string
	SampleRate int
	Timescale  int64
	Segments   []Segment
	Init       *SegRef
	Duration   media.Duration
	Period     Period
}

// Dash holds all resolved tracks of an MPD.
type Dash struct {
	Artifact string
	MPD      MPD
	Tracks   []DashTrack
	Lines    []Line
}

// ParseDashMPD decodes the MPD with encoding/xml, then resolves every
// representation against the input root and the exact rational clock.
func ParseDashMPD(art Artifact, opt HSLOptions) (*Dash, error) {
	if !opt.Clock.Valid() {
		return nil, fmt.Errorf("manifest: HSLOptions.Clock required")
	}
	lines := ScanLines(art.Data)
	dec := xml.NewDecoder(bytes.NewReader(art.Data))
	var mpd MPD
	if err := dec.Decode(&mpd); err != nil {
		return nil, media.WrapFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name}, err)
	}
	if len(mpd.Periods) == 0 {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name}, "MPD has no Period")
	}
	d := &Dash{Artifact: art.Name, MPD: mpd, Lines: lines}

	if mpd.MediaPresentationDuration != "" && opt.MaxDurationSec > 0 {
		v, err := ParseISODuration(mpd.MediaPresentationDuration)
		if err != nil {
			return nil, media.WrapFault(media.FaultDurationMismatch, media.SourcePos{Artifact: art.Name}, err)
		}
		if v.Seconds() > opt.MaxDurationSec+0.5 {
			return nil, media.NewFault(media.FaultDurationMismatch, media.SourcePos{Artifact: art.Name},
				fmt.Sprintf("MPD duration %s exceeds limit %.1fs", mpd.MediaPresentationDuration, opt.MaxDurationSec))
		}
	}

	for _, per := range mpd.Periods {
		periodPos := media.SourcePos{Artifact: art.Name, Segment: -1}
		base := chainBase(mpd.BaseURL, per.BaseURL)
		var periodDur media.Duration
		for _, src := range []string{per.Duration, mpd.MediaPresentationDuration} {
			if src == "" {
				continue
			}
			v, err := ParseISODuration(src)
			if err != nil {
				return nil, media.WrapFault(media.FaultDurationMismatch, periodPos, err)
			}
			dur, err := v.Convert(opt.Clock)
			if err != nil {
				return nil, media.WrapFault(media.FaultDurationMismatch, periodPos, err)
			}
			periodDur = dur
			break
		}
		for _, as := range per.AdaptationSets {
			kind := adaptationKind(as)
			if kind == 0 {
				continue
			}
			setCodecs := as.Codecs
			for ri, rep := range as.Representations {
				pos := media.SourcePos{Artifact: art.Name, Segment: ri, Line: 1}
				if l := findLineContaining(lines, `id="`+rep.ID+`"`); l.Num > 0 {
					pos.Line, pos.Offset = l.Num, l.Offset
				}
				codecs := rep.Codecs
				if codecs == "" {
					codecs = setCodecs
				}
				tmpl := rep.SegmentTemplate
				if tmpl == nil {
					tmpl = as.SegmentTemplate
				}
				if tmpl == nil {
					return nil, media.NewFault(media.FaultUnsupportedCodec, pos,
						"representation without SegmentTemplate is not supported")
				}
				track, err := resolveRepresentation(art.Name, kind, rep, tmpl, chainBase(base, as.BaseURL, rep.BaseURL), periodDur, lines, opt)
				if err != nil {
					return nil, media.WrapFault(media.FaultManifestRange, pos, err)
				}
				if track.Codecs == "" {
					track.Codecs = codecs
				}
				d.Tracks = append(d.Tracks, track)
			}
		}
	}
	if len(d.Tracks) == 0 {
		return nil, media.NewFault(media.FaultManifestSyntax, media.SourcePos{Artifact: art.Name},
			"MPD has no video or audio representation")
	}
	return d, nil
}

func adaptationKind(as AdaptationSet) media.Kind {
	switch {
	case strings.HasPrefix(as.MimeType, "video/"):
		return media.KindVideo
	case strings.HasPrefix(as.MimeType, "audio/"):
		return media.KindAudio
	case as.ContentType == "video":
		return media.KindVideo
	case as.ContentType == "audio":
		return media.KindAudio
	}
	return 0
}

func chainBase(parts ...string) string {
	var segs []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			segs = append(segs, p)
		}
	}
	return joinURIPath(segs...)
}

// joinURIPath joins root-relative path fragments without allowing escape.
func joinURIPath(parts ...string) string {
	joined := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if joined == "" {
			joined = p
		} else {
			joined = strings.TrimSuffix(joined, "/") + "/" + p
		}
	}
	return joined
}

func resolveRepresentation(artifact string, kind media.Kind, rep Representation, tmpl *SegmentTemplate, base string, periodDur media.Duration, lines []Line, opt HSLOptions) (DashTrack, error) {
	timescale := int64(1000)
	if tmpl.Timescale != "" {
		ts, err := strconv.ParseInt(tmpl.Timescale, 10, 64)
		if err != nil || ts <= 0 {
			return DashTrack{}, fmt.Errorf("bad SegmentTemplate@timescale %q", tmpl.Timescale)
		}
		timescale = ts
	}
	startNumber := int64(1)
	if tmpl.StartNumber != "" {
		n, err := strconv.ParseInt(tmpl.StartNumber, 10, 64)
		if err != nil {
			return DashTrack{}, fmt.Errorf("bad startNumber %q", tmpl.StartNumber)
		}
		startNumber = n
	}
	var pto int64
	if tmpl.PresentationTimeOffset != "" {
		v, err := strconv.ParseInt(tmpl.PresentationTimeOffset, 10, 64)
		if err != nil {
			return DashTrack{}, fmt.Errorf("bad presentationTimeOffset %q", tmpl.PresentationTimeOffset)
		}
		pto = v
	}

	track := DashTrack{
		Kind: kind, RepID: rep.ID, Codecs: rep.Codecs, MimeType: rep.MimeType,
		FrameRate: rep.FrameRate, Timescale: timescale,
	}
	if bw, err := strconv.ParseInt(rep.Bandwidth, 10, 64); err == nil {
		track.Bandwidth = bw
	}
	if kind == media.KindAudio && rep.AudioSamplingRate != "" {
		if r, err := strconv.Atoi(strings.Split(rep.AudioSamplingRate, ",")[0]); err == nil {
			track.SampleRate = r
		}
	}

	initRef := func(raw string) (*SegRef, error) {
		raw = strings.TrimSpace(raw)
		raw = strings.ReplaceAll(raw, "$RepresentationID$", rep.ID)
		if raw == "" {
			return nil, media.NewFault(media.FaultMissingInit, media.SourcePos{Artifact: artifact},
				"SegmentTemplate has no initialization")
		}
		l := findLineContaining(lines, raw)
		clean, err := ValidateURI(opt.Root, joinURIPath(base, raw))
		if err != nil {
			return nil, err
		}
		return &SegRef{URI: clean, Raw: raw, Line: l.Num}, nil
	}
	if tmpl.Initialization != "" {
		if err := addInit(&track, tmpl.Initialization, initRef); err != nil {
			return DashTrack{}, err
		}
	}

	var entries []SegmentEntry
	if tmpl.SegmentTimeline != nil {
		entries = tmpl.SegmentTimeline.Entries
	} else if tmpl.Duration != "" {
		dur, err := strconv.ParseInt(tmpl.Duration, 10, 64)
		if err != nil || dur <= 0 {
			return DashTrack{}, fmt.Errorf("bad segment duration %q", tmpl.Duration)
		}
		if periodDur.Clock().Valid() && periodDur.Ticks() > 0 {
			segDur, err := convertTicks(dur, timescale, opt.Clock)
			if err != nil {
				return DashTrack{}, err
			}
			if segDur.Ticks() <= 0 {
				return DashTrack{}, fmt.Errorf("non-positive segment duration")
			}
			rep := (periodDur.Ticks()+segDur.Ticks()-1)/segDur.Ticks() - 1
			if rep > 100000 {
				return DashTrack{}, fmt.Errorf("segment count %d exceeds limit", rep+1)
			}
			entries = []SegmentEntry{{D: strconv.FormatInt(dur, 10), R: strconv.FormatInt(rep, 10)}}
		} else {
			entries = []SegmentEntry{{D: strconv.FormatInt(dur, 10)}}
		}
	} else {
		return DashTrack{}, fmt.Errorf("SegmentTemplate has neither SegmentTimeline nor duration")
	}

	clock := opt.Clock
	total := int64(0)
	seq := startNumber
	var prevEnd int64
	for _, e := range entries {
		var tStart int64
		if e.T != "" {
			v, err := strconv.ParseInt(e.T, 10, 64)
			if err != nil {
				return DashTrack{}, fmt.Errorf("bad S@t %q", e.T)
			}
			tStart = v
		} else {
			tStart = prevEnd
		}
		dv, err := strconv.ParseInt(e.D, 10, 64)
		if err != nil || dv <= 0 {
			return DashTrack{}, fmt.Errorf("bad S@d %q", e.D)
		}
		repeat := int64(0)
		if e.R != "" {
			repeat, err = strconv.ParseInt(e.R, 10, 64)
			if err != nil || repeat < 0 {
				return DashTrack{}, fmt.Errorf("bad S@r %q", e.R)
			}
		}
		if e.N != "" {
			n, err := strconv.ParseInt(e.N, 10, 64)
			if err != nil {
				return DashTrack{}, fmt.Errorf("bad S@n %q", e.N)
			}
			seq = n
		}
		durTicks, err := convertTicks(dv, timescale, clock)
		if err != nil {
			return DashTrack{}, fmt.Errorf("segment duration: %w", err)
		}
		if durTicks.Ticks() <= 0 {
			return DashTrack{}, fmt.Errorf("non-positive segment duration")
		}
		for i := int64(0); i <= repeat; i++ {
			segStart := tStart + int64(i)*dv - pto
			if segStart < 0 {
				return DashTrack{}, fmt.Errorf("negative segment start %d", segStart)
			}
			rawMedia := strings.NewReplacer(
				"$RepresentationID$", rep.ID,
				"$Number$", strconv.FormatInt(seq, 10),
				"$Time$", strconv.FormatInt(segStart+pto, 10),
				"$Number%05d$", fmt.Sprintf("%05d", seq),
			).Replace(tmpl.Media)
			l := findLineContaining(lines, rawMedia)
			clean, err := ValidateURI(opt.Root, joinURIPath(base, rawMedia))
			if err != nil {
				return DashTrack{}, err
			}
			track.Segments = append(track.Segments, Segment{
				Seq:      seq,
				Ref:      SegRef{URI: clean, Raw: rawMedia, Line: l.Num, Offset: l.Offset},
				Duration: durTicks, DurationSec: durTicks.Seconds(),
				TotalTicks: segStart,
				Init:       track.Init,
			})
			prevEnd = tStart + int64(i+1)*dv
			seq++
			total += durTicks.Ticks()
		}
	}
	if len(track.Segments) == 0 {
		return DashTrack{}, fmt.Errorf("representation produced no segments")
	}
	for i := range track.Segments {
		track.Segments[i].Init = track.Init
	}
	// total already accumulates clock ticks, not timescale ticks.
	track.Duration = clock.FromTicks(total)
	return track, nil
}

func addInit(track *DashTrack, raw string, fn func(string) (*SegRef, error)) error {
	ref, err := fn(raw)
	if err != nil {
		return err
	}
	track.Init = ref
	return nil
}

func convertTicks(ticks, timescale int64, clock media.Clock) (media.Duration, error) {
	num := ticks * clock.TicksPerSecond()
	if num%timescale != 0 {
		return media.Duration{}, fmt.Errorf("%d ticks @%d not exact at %d", ticks, timescale, clock.TicksPerSecond())
	}
	return clock.FromTicks(num / timescale), nil
}
