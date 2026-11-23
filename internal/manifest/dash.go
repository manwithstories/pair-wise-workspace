package manifest

import (
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"

	"mediascan/internal/clock"
)

// dashMPD mirrors the subset of the DASH MPD schema this tool consumes.
type dashMPD struct {
	XMLName                   xml.Name     `xml:"MPD"`
	Type                      string       `xml:"type,attr"`
	MediaPresentationDuration string       `xml:"mediaPresentationDuration,attr"`
	Periods                   []dashPeriod `xml:"Period"`
}

type dashPeriod struct {
	ID             string           `xml:"id,attr"`
	Duration       string           `xml:"duration,attr"`
	AdaptationSets []dashAdaptation `xml:"AdaptationSet"`
}

type dashAdaptation struct {
	ID              string        `xml:"id,attr"`
	ContentType     string        `xml:"contentType,attr"`
	MimeType        string        `xml:"mimeType,attr"`
	Codecs          string        `xml:"codecs,attr"`
	Lang            string        `xml:"lang,attr"`
	SegmentTemplate *dashTemplate `xml:"SegmentTemplate"`
	SegmentList     *dashSegList  `xml:"SegmentList"`
	Reps            []dashRep     `xml:"Representation"`
}

type dashRep struct {
	ID              string        `xml:"id,attr"`
	Bandwidth       int64         `xml:"bandwidth,attr"`
	MimeType        string        `xml:"mimeType,attr"`
	Codecs          string        `xml:"codecs,attr"`
	Width           int           `xml:"width,attr"`
	Height          int           `xml:"height,attr"`
	AudioChannels   int           `xml:"audioSamplingRate,attr"`
	SegmentTemplate *dashTemplate `xml:"SegmentTemplate"`
	SegmentList     *dashSegList  `xml:"SegmentList"`
}

type dashTemplate struct {
	Timescale      int64  `xml:"timescale,attr"`
	Duration       int64  `xml:"duration,attr"`
	StartNumber    *int64 `xml:"startNumber,attr"`
	Media          string `xml:"media,attr"`
	Initialization string `xml:"initialization,attr"`
}

type dashSegList struct {
	Timescale   int64       `xml:"timescale,attr"`
	Duration    int64       `xml:"duration,attr"`
	SegmentURL  []dashURL   `xml:"SegmentURL"`
	Init        *dashURLRef `xml:"Initialization"`
	StartNumber *int64      `xml:"startNumber,attr"`
}

type dashURL struct {
	Media      string `xml:"media,attr"`
	MediaRange string `xml:"mediaRange,attr"`
}

type dashURLRef struct {
	SourceURL string `xml:"sourceURL,attr"`
	Range     string `xml:"range,attr"`
}

func parseDASHGroup(data []byte, opts Options) (*Group, error) {
	opts = opts.normalize()
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	var mpd dashMPD
	if err := dec.Decode(&mpd); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	mpdDur, err := parseISO8601Duration(mpd.MediaPresentationDuration)
	if err != nil {
		return nil, err
	}
	if len(mpd.Periods) == 0 {
		return nil, fmt.Errorf("%w: MPD has no Period", ErrInvalid)
	}
	g := &Group{Format: FormatDASH}
	var total int64
	for pi := range mpd.Periods {
		per := &mpd.Periods[pi]
		perDur := mpdDur
		if per.Duration != "" {
			perDur, err = parseISO8601Duration(per.Duration)
			if err != nil {
				return nil, err
			}
		}
		if perDur <= 0 {
			return nil, fmt.Errorf("%w: Period %q has no duration", ErrInvalid, per.ID)
		}
		for ai := range per.AdaptationSets {
			as := &per.AdaptationSets[ai]
			for ri := range as.Reps {
				pl, err := buildDASHPlan(as, &as.Reps[ri], perDur, pi, opts)
				if err != nil {
					return nil, fmt.Errorf("representation %s: %w", as.Reps[ri].ID, err)
				}
				g.Plans = append(g.Plans, pl)
			}
		}
		total += perDur
	}
	g.DurationNs = total
	if err := finalizeGroup(g, opts); err != nil {
		return nil, err
	}
	// DASH representations need not be co-duration across tracks, so the
	// shared-duration check is relaxed to the MPD total.
	g.DurationNs = total
	return g, nil
}

func buildDASHPlan(as *dashAdaptation, rep *dashRep, periodDur int64, periodIdx int, opts Options) (*Plan, error) {
	pl := &Plan{
		Format:         FormatDASH,
		Representation: rep.ID,
		Bandwidth:      rep.Bandwidth,
		MimeType:       firstNonEmpty(rep.MimeType, as.MimeType),
		Codecs:         firstNonEmpty(rep.Codecs, as.Codecs),
		Timescale:      clock.DefaultTimescale,
	}
	if pl.Representation == "" {
		pl.Representation = fmt.Sprintf("p%d-r%d", periodIdx, rep.Bandwidth)
	}
	tmpl := rep.SegmentTemplate
	if tmpl == nil {
		tmpl = as.SegmentTemplate
	}
	list := rep.SegmentList
	if list == nil {
		list = as.SegmentList
	}
	switch {
	case tmpl != nil:
		if err := expandTemplate(pl, tmpl, periodDur, opts); err != nil {
			return nil, err
		}
	case list != nil:
		if err := expandList(pl, list, periodDur, opts); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: no SegmentTemplate or SegmentList", ErrInvalid)
	}
	for _, s := range pl.Segments {
		pl.DurationNs += s.DurationNs
	}
	return pl, nil
}

func expandTemplate(pl *Plan, t *dashTemplate, periodDur int64, opts Options) error {
	if t.Media == "" {
		return fmt.Errorf("%w: SegmentTemplate without media", ErrInvalid)
	}
	ts := t.Timescale
	if ts <= 0 {
		ts = int64(clock.DefaultTimescale)
	}
	pl.Timescale = uint32(ts)
	if t.Duration <= 0 {
		return fmt.Errorf("%w: SegmentTemplate without duration", ErrInvalid)
	}
	segDur := clock.NanosDelta(t.Duration, uint32(ts))
	if segDur <= 0 {
		return fmt.Errorf("%w: non-positive segment duration", ErrInvalid)
	}
	n := int(math.Ceil(float64(periodDur) / float64(segDur)))
	if n <= 0 {
		return fmt.Errorf("%w: no segments", ErrInvalid)
	}
	if n > opts.MaxSegments {
		return fmt.Errorf("%w: %d segments", ErrTooLarge, n)
	}
	start := int64(1)
	if t.StartNumber != nil {
		start = *t.StartNumber
	}
	initURI := ""
	if t.Initialization != "" {
		initURI = expandTemplateString(t.Initialization, 0, pl.Bandwidth, pl.Representation)
		if err := ValidateURI(initURI); err != nil {
			return err
		}
	}
	for i := 0; i < n; i++ {
		num := start + int64(i)
		uri := expandTemplateString(t.Media, num, pl.Bandwidth, pl.Representation)
		if err := ValidateURI(uri); err != nil {
			return err
		}
		dur := segDur
		if rem := periodDur - int64(i)*segDur; rem < dur {
			dur = rem
		}
		pl.Segments = append(pl.Segments, Segment{
			URI:        uri,
			Sequence:   num,
			StartNs:    int64(i) * segDur,
			DurationNs: dur,
			InitURI:    initURI,
		})
	}
	return nil
}

func expandList(pl *Plan, l *dashSegList, periodDur int64, opts Options) error {
	if len(l.SegmentURL) == 0 {
		return fmt.Errorf("%w: SegmentList without SegmentURL", ErrInvalid)
	}
	if len(l.SegmentURL) > opts.MaxSegments {
		return fmt.Errorf("%w: %d segment URLs", ErrTooLarge, len(l.SegmentURL))
	}
	ts := l.Timescale
	if ts <= 0 {
		ts = int64(clock.DefaultTimescale)
	}
	pl.Timescale = uint32(ts)
	var segDur int64
	if l.Duration > 0 {
		segDur = clock.NanosDelta(l.Duration, uint32(ts))
	} else {
		segDur = periodDur / int64(len(l.SegmentURL))
	}
	if segDur <= 0 {
		return fmt.Errorf("%w: non-positive segment duration", ErrInvalid)
	}
	initURI := ""
	if l.Init != nil && l.Init.SourceURL != "" {
		initURI = l.Init.SourceURL
		if err := ValidateURI(initURI); err != nil {
			return err
		}
	}
	start := int64(1)
	if l.StartNumber != nil {
		start = *l.StartNumber
	}
	var cursor int64
	for i, u := range l.SegmentURL {
		if err := ValidateURI(u.Media); err != nil {
			return err
		}
		dur := segDur
		if i == len(l.SegmentURL)-1 {
			dur = periodDur - cursor
		}
		br, err := parseRangeHeader(u.MediaRange)
		if err != nil {
			return err
		}
		pl.Segments = append(pl.Segments, Segment{
			URI:        u.Media,
			Sequence:   start + int64(i),
			StartNs:    cursor,
			DurationNs: dur,
			ByteRange:  br,
			InitURI:    initURI,
		})
		cursor += segDur
	}
	return nil
}

func parseRangeHeader(v string) (*ByteRange, error) {
	if v == "" {
		return nil, nil
	}
	parts := strings.SplitN(v, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: bad byte range %q", ErrInvalid, v)
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return nil, fmt.Errorf("%w: bad byte range %q", ErrInvalid, v)
	}
	return &ByteRange{Offset: start, Length: end - start + 1}, nil
}
