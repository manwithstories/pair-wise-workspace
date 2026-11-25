package manifest

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"

	"mediad/internal/media"
)

// mpdDoc mirrors the subset of the DASH MPD schema the kernel understands.
type mpdDoc struct {
	XMLName                   xml.Name    `xml:"MPD"`
	Type                      string      `xml:"type,attr"`
	MediaPresentationDuration string      `xml:"mediaPresentationDuration,attr"`
	Periods                   []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	ID          string          `xml:"id,attr"`
	Duration    string          `xml:"duration,attr"`
	BaseURL     string          `xml:"BaseURL"`
	Adaptations []mpdAdaptation `xml:"AdaptationSet"`
}

type mpdAdaptation struct {
	ID              string              `xml:"id,attr"`
	Lang            string              `xml:"lang,attr"`
	ContentType     string              `xml:"contentType,attr"`
	MimeType        string              `xml:"mimeType,attr"`
	Codecs          string              `xml:"codecs,attr"`
	Role            string              `xml:"Role>value"`
	SegmentTemplate *mpdSegmentTemplate `xml:"SegmentTemplate"`
	SegmentList     *mpdSegmentList     `xml:"SegmentList"`
	Representations []mpdRepresentation `xml:"Representation"`
}

type mpdRepresentation struct {
	ID              string              `xml:"id,attr"`
	Bandwidth       int                 `xml:"bandwidth,attr"`
	Width           int                 `xml:"width,attr"`
	Height          int                 `xml:"height,attr"`
	Codecs          string              `xml:"codecs,attr"`
	BaseURL         string              `xml:"BaseURL"`
	SegmentTemplate *mpdSegmentTemplate `xml:"SegmentTemplate"`
	SegmentList     *mpdSegmentList     `xml:"SegmentList"`
}

type mpdSegmentTemplate struct {
	Timescale      int64  `xml:"timescale,attr"`
	Duration       int64  `xml:"duration,attr"`
	StartNumber    int64  `xml:"startNumber,attr"`
	Media          string `xml:"media,attr"`
	Initialization string `xml:"initialization,attr"`
}

type mpdSegmentList struct {
	Duration       int64           `xml:"duration,attr"`
	Timescale      int64           `xml:"timescale,attr"`
	Initialization string          `xml:"Initialization>sourceURL,attr"`
	SegmentURLs    []mpdSegmentURL `xml:"SegmentURL"`
}

type mpdSegmentURL struct {
	Media      string `xml:"media,attr"`
	MediaRange string `xml:"mediaRange,attr"`
}

// lineTracker records the byte offset of each newline so XML decode errors
// and element starts can be reported as file:line:col.
type lineTracker struct {
	offsets []int
}

func newLineTracker(data []byte) *lineTracker {
	lt := &lineTracker{offsets: []int{0}}
	for i, b := range data {
		if b == '\n' {
			lt.offsets = append(lt.offsets, i+1)
		}
	}
	return lt
}

// posFromOffset converts a byte offset into a 1-based line and column.
func (lt *lineTracker) posFromOffset(file string, off int64) media.SourcePos {
	lo, hi := 0, len(lt.offsets)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if int64(lt.offsets[mid]) <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	col := int(off) - lt.offsets[lo] + 1
	return media.SourcePos{File: file, Line: lo + 1, Col: col}
}

// ReadDASH parses a DASH MPD and expands SegmentTemplate/SegmentList addresses
// into a flat segment list. Every segment is validated against the local root.
func ReadDASH(file string, opts Options) (*Manifest, error) {
	data, err := readLimited(file, opts.limits())
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(data, []byte("<MPD")) {
		return nil, media.New(media.CodeManifest, "dash", media.SourcePos{File: file, Line: 1, Col: 1},
			"missing MPD root element")
	}
	lt := newLineTracker(data)
	var doc mpdDoc
	dec := xml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		pos := media.SourcePos{File: file, Line: 1, Col: 1}
		if se, ok := err.(*xml.SyntaxError); ok {
			pos.Line = se.Line
		} else if dec.InputOffset() > 0 {
			pos = lt.posFromOffset(file, dec.InputOffset())
		}
		return nil, media.Wrap(media.CodeManifest, "dash", pos, err, "malformed MPD")
	}

	lim := opts.limits()
	out := &Manifest{Format: "dash", File: file, Timescale: media.TickRate}
	seq := 0
	for pi := range doc.Periods {
		period := &doc.Periods[pi]
		periodStart := int64(0)
		for ai := range period.Adaptations {
			ad := &period.Adaptations[ai]
			reps := ad.Representations
			if len(reps) == 0 {
				return nil, media.New(media.CodeMissingEntry, "dash",
					media.SourcePos{File: file, Line: 1, Col: 1},
					"AdaptationSet %q declares no Representation", ad.ID)
			}
			for ri := range reps {
				rep := &reps[ri]
				tmpl := firstTemplate(rep.SegmentTemplate, ad.SegmentTemplate)
				list := firstList(rep.SegmentList, ad.SegmentList)
				if tmpl == nil && list == nil {
					continue
				}
				codec := firstNonEmpty(rep.Codecs, ad.Codecs, "avc1")
				rend := &Rendition{
					Camera:    cameraForRep(rep.ID, ad.ID),
					Language:  defaultString(ad.Lang, "und"),
					Codec:     codec,
					Role:      defaultString(ad.Role, roleForAdaptation(ad)),
					Bandwidth: rep.Bandwidth,
					Width:     rep.Width,
					Height:    rep.Height,
					Type:      contentType(ad),
				}
				out.Renditions = append(out.Renditions, rend)
				segs, err := expandRepresentation(file, period, rep, ad, tmpl, list, lt, opts, periodStart)
				if err != nil {
					return nil, err
				}
				for si := range segs {
					s := segs[si]
					s.Seq = seq
					s.Rendition = rend
					seq++
					out.Segments = append(out.Segments, s)
					if err := checkSegLimit(lim, len(out.Segments), s.Pos); err != nil {
						return nil, err
					}
				}
				if tmpl != nil && tmpl.Duration > 0 && tmpl.Timescale > 0 {
					periodStart += tmpl.Duration * int64(len(segs))
				}
			}
		}
	}
	if len(out.Segments) == 0 {
		return nil, media.New(media.CodeMissingEntry, "dash", media.SourcePos{File: file, Line: 1, Col: 1},
			"no segments expandable from MPD")
	}
	if err := ValidateSegments(out.Segments); err != nil {
		return nil, err
	}
	return out, nil
}

func expandRepresentation(file string, period *mpdPeriod, rep *mpdRepresentation, ad *mpdAdaptation,
	tmpl *mpdSegmentTemplate, list *mpdSegmentList, lt *lineTracker, opts Options, start int64) ([]Segment, error) {
	base := firstNonEmpty(rep.BaseURL, period.BaseURL, "")
	var segs []Segment
	switch {
	case list != nil && len(list.SegmentURLs) > 0:
		timescale := list.Timescale
		if timescale <= 0 {
			timescale = 90000
		}
		for i, su := range list.SegmentURLs {
			if su.Media == "" {
				return nil, media.New(media.CodeMissingEntry, "dash",
					media.SourcePos{File: file, Line: 1, Col: 1}, "SegmentURL #%d has no media", i)
			}
			uri := joinBase(base, su.Media)
			_, rel, err := ResolveURI(opts.Root, file, uri, media.SourcePos{File: file, Line: 1, Col: 1})
			if err != nil {
				return nil, err
			}
			dur := media.Rational{Num: 0, Den: 1}
			if list.Duration > 0 {
				dur = media.MustRational(list.Duration, timescale)
			}
			segs = append(segs, Segment{
				Seq: i, URI: uri, RelPath: rel, Duration: dur,
				StartTicks: start + int64(i)*list.Duration,
				Pos:        media.SourcePos{File: file, Line: 1, Col: 1},
			})
		}
		return segs, nil
	case tmpl != nil:
		if tmpl.Duration <= 0 {
			return nil, media.New(media.CodeInvalidDuration, "dash",
				media.SourcePos{File: file, Line: 1, Col: 1}, "SegmentTemplate has no @duration")
		}
		timescale := tmpl.Timescale
		if timescale <= 0 {
			timescale = 90000
		}
		count, err := segmentCount(period, tmpl, timescale)
		if err != nil {
			return nil, err
		}
		if count <= 0 {
			return nil, media.New(media.CodeMissingEntry, "dash",
				media.SourcePos{File: file, Line: 1, Col: 1}, "SegmentTemplate expands to zero segments")
		}
		startNum := tmpl.StartNumber
		for i := 0; i < count; i++ {
			number := startNum + int64(i)
			mediaPath := expandTemplate(tmpl.Media, rep.ID, rep.Bandwidth, number)
			if mediaPath == "" {
				return nil, media.New(media.CodeMissingEntry, "dash",
					media.SourcePos{File: file, Line: 1, Col: 1}, "empty media template")
			}
			uri := joinBase(base, mediaPath)
			_, rel, err := ResolveURI(opts.Root, file, uri, media.SourcePos{File: file, Line: 1, Col: 1})
			if err != nil {
				return nil, err
			}
			dur := media.MustRational(tmpl.Duration, timescale)
			segs = append(segs, Segment{
				Seq:        i,
				URI:        uri,
				RelPath:    rel,
				Duration:   dur,
				StartTicks: start + int64(i)*tmpl.Duration,
				Pos:        media.SourcePos{File: file, Line: 1, Col: 1},
			})
		}
		return segs, nil
	}
	return nil, nil
}

// segmentCount determines how many segments a template expands to, using the
// period duration when present. Falls back to a bounded default.
func segmentCount(period *mpdPeriod, tmpl *mpdSegmentTemplate, timescale int64) (int, error) {
	dur := period.Duration
	if dur == "" {
		dur = "PT30S"
	}
	d, err := parseISODuration(dur)
	if err != nil {
		return 0, media.New(media.CodeInvalidDuration, "dash",
			media.SourcePos{}, "invalid period duration %q", dur)
	}
	periodTicks, err := d.RescaleTicks(timescale)
	if err != nil {
		return 0, media.Wrap(media.CodeInvalidDuration, "dash", media.SourcePos{}, err, "")
	}
	if tmpl.Duration <= 0 {
		return 0, nil
	}
	n := (periodTicks + tmpl.Duration - 1) / tmpl.Duration
	if n > 1<<20 {
		return 0, media.New(media.CodeLimit, "dash", media.SourcePos{}, "segment count %d too large", n)
	}
	return int(n), nil
}

func parseISODuration(s string) (media.Rational, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "P") {
		return media.Rational{}, errZeroDuration
	}
	body := strings.TrimPrefix(s, "P")
	var days, hours, mins int64
	var secs float64
	var datePart, timePart string
	if i := strings.IndexByte(body, 'T'); i >= 0 {
		datePart, timePart = body[:i], body[i+1:]
	} else {
		datePart = body
	}
	parseDate := func(part string) error {
		for part != "" {
			i := 0
			for i < len(part) && (part[i] >= '0' && part[i] <= '9' || part[i] == '.') {
				i++
			}
			if i == 0 || i >= len(part) {
				return errZeroDuration
			}
			v, err := strconv.ParseFloat(part[:i], 64)
			if err != nil {
				return errZeroDuration
			}
			switch part[i] {
			case 'D':
				days = int64(v)
			case 'W':
				days = int64(v) * 7
			default:
				return errZeroDuration
			}
			part = part[i+1:]
		}
		return nil
	}
	parseTime := func(part string) error {
		for part != "" {
			i := 0
			for i < len(part) && (part[i] >= '0' && part[i] <= '9' || part[i] == '.') {
				i++
			}
			if i == 0 || i >= len(part) {
				return errZeroDuration
			}
			v, err := strconv.ParseFloat(part[:i], 64)
			if err != nil {
				return errZeroDuration
			}
			switch part[i] {
			case 'H':
				hours = int64(v)
			case 'M':
				mins = int64(v)
			case 'S':
				secs = v
			default:
				return errZeroDuration
			}
			part = part[i+1:]
		}
		return nil
	}
	if err := parseDate(datePart); err != nil {
		return media.Rational{}, err
	}
	if timePart != "" {
		if err := parseTime(timePart); err != nil {
			return media.Rational{}, err
		}
	}
	totalMS := ((days*24+hours)*60+mins)*60*1000 + int64(secs*1000+0.5)
	if totalMS <= 0 {
		return media.Rational{}, errZeroDuration
	}
	return media.MustRational(totalMS, 1000), nil
}

func expandTemplate(tmpl, repID string, bandwidth int, number int64) string {
	if tmpl == "" {
		return ""
	}
	r := strings.NewReplacer(
		"$RepresentationID$", repID,
		"$Bandwidth$", strconv.Itoa(bandwidth),
		"$Number$", strconv.FormatInt(number, 10),
		"$Number%05d$", pad(number, 5),
		"$Time$", "",
	)
	return r.Replace(tmpl)
}

func pad(n int64, width int) string {
	s := strconv.FormatInt(n, 10)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

func joinBase(base, ref string) string {
	if base == "" {
		return ref
	}
	if strings.HasPrefix(ref, "/") {
		return strings.TrimSuffix(base, "/") + ref
	}
	if strings.HasSuffix(base, "/") {
		return base + ref
	}
	return base + "/" + ref
}

func firstTemplate(a, b *mpdSegmentTemplate) *mpdSegmentTemplate {
	if a != nil {
		return a
	}
	return b
}

func firstList(a, b *mpdSegmentList) *mpdSegmentList {
	if a != nil {
		return a
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func cameraForRep(repID, adID string) string {
	if strings.HasPrefix(repID, "cam") {
		return repID
	}
	if strings.HasPrefix(adID, "cam") {
		return adID
	}
	return firstNonEmpty(repID, adID, "cam1")
}

func roleForAdaptation(ad *mpdAdaptation) string {
	low := strings.ToLower(ad.ID + " " + ad.ContentType)
	switch {
	case strings.Contains(low, "side"):
		return "side"
	case strings.Contains(low, "roam"):
		return "roaming"
	default:
		return "main"
	}
}

func contentType(ad *mpdAdaptation) string {
	if strings.Contains(strings.ToLower(ad.MimeType), "audio") {
		return "audio"
	}
	if strings.Contains(strings.ToLower(ad.MimeType), "video") {
		return "video"
	}
	return defaultString(ad.ContentType, "video")
}

var errZeroDuration = &media.Error{Code: media.CodeInvalidDuration, Detail: "non-positive duration"}
