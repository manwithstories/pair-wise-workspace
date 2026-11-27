package manifest

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"avrepair/internal/media"
)

// mpdDocument mirrors the subset of the DASH MPD schema needed to build a
// local timeline. encoding/xml ignores unknown attributes and elements.
type mpdDocument struct {
	XMLName xml.Name    `xml:"MPD"`
	Type    string      `xml:"type,attr"`
	Periods []mpdPeriod `xml:"Period"`
	BaseURL string      `xml:"BaseURL"`
}

type mpdPeriod struct {
	ID             string        `xml:"id,attr"`
	Start          string        `xml:"start,attr"`
	Duration       string        `xml:"duration,attr"`
	AdaptationSets []mpdAdaptSet `xml:"AdaptationSet"`
}

type mpdAdaptSet struct {
	ID              string         `xml:"id,attr"`
	ContentType     string         `xml:"contentType,attr"`
	MimeType        string         `xml:"mimeType,attr"`
	Lang            string         `xml:"lang,attr"`
	SegmentTemplate *mpdTemplate   `xml:"SegmentTemplate"`
	Representations []mpdRepresent `xml:"Representation"`
}

type mpdRepresent struct {
	ID              string       `xml:"id,attr"`
	Codecs          string       `xml:"codecs,attr"`
	Bandwidth       int64        `xml:"bandwidth,attr"`
	MimeType        string       `xml:"mimeType,attr"`
	ContentType     string       `xml:"contentType,attr"`
	SegmentTemplate *mpdTemplate `xml:"SegmentTemplate"`
	BaseURL         string       `xml:"BaseURL"`
}

type mpdTemplate struct {
	Media          string       `xml:"media,attr"`
	Initialization string       `xml:"initialization,attr"`
	Timescale      int64        `xml:"timescale,attr"`
	Duration       int64        `xml:"duration,attr"`
	StartNumber    *int64       `xml:"startNumber,attr"`
	PresentationTO int64        `xml:"presentationTimeOffset,attr"`
	Timeline       *mpdTimeline `xml:"SegmentTimeline"`
}

type mpdTimeline struct {
	Segments []mpdS `xml:"S"`
}

type mpdS struct {
	T int64  `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int64  `xml:"r,attr"`
	N *int64 `xml:"n,attr"`
}

const mpdTimescaleDefault int64 = 1

// ParseDASH builds the entry list for every representation in the first
// period. It rejects zero or negative timescales and oversized timelines.
func ParseDASH(path string, data []byte) (*Manifest, error) {
	var doc mpdDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("dash: %s: %w", filepath.Base(path), err)
	}
	if len(doc.Periods) == 0 {
		return nil, fmt.Errorf("dash: %s has no periods", filepath.Base(path))
	}
	baseDir := filepath.Dir(path)
	baseURL, err := dashBaseURL(baseDir, doc.BaseURL)
	if err != nil {
		return nil, err
	}
	period := doc.Periods[0]
	out := &Manifest{Kind: "dash", Source: filepath.Clean(path)}
	for ai, as := range period.AdaptationSets {
		for ri, rep := range as.Representations {
			tmpl := rep.SegmentTemplate
			if tmpl == nil {
				tmpl = as.SegmentTemplate
			}
			if tmpl == nil {
				out.Warnings = append(out.Warnings, fmt.Sprintf("dash: adaptation set %d representation %d has no SegmentTemplate", ai, ri))
				continue
			}
			timescale := tmpl.Timescale
			if timescale == 0 {
				timescale = mpdTimescaleDefault
			}
			if timescale < 0 {
				return nil, &media.FaultError{Kind: media.ErrInvalidTimescale, Detail: fmt.Sprintf("representation %s timescale %d", rep.ID, timescale)}
			}
			kind := dashKind(as, rep)
			track := dashTrack(as, rep, kind)
			tl := tmpl.Timeline
			entries, err := dashEntries(tmpl, tl, timescale, kind, track, baseURL, rep.ID)
			if err != nil {
				return nil, err
			}
			out.Entries = append(out.Entries, entries...)
			if out.Timescale == 0 {
				out.Timescale = timescale
			}
		}
	}
	if len(out.Entries) == 0 {
		return nil, fmt.Errorf("dash: %s has no timeline entries", filepath.Base(path))
	}
	return out, nil
}

func dashBaseURL(dir, base string) (string, error) {
	if strings.TrimSpace(base) == "" {
		return dir, nil
	}
	return media.ResolveLocalURI(dir, strings.TrimSpace(base))
}

func dashKind(as mpdAdaptSet, rep mpdRepresent) media.TrackType {
	for _, v := range []string{rep.ContentType, as.ContentType, rep.MimeType, as.MimeType} {
		v = strings.ToLower(v)
		switch {
		case strings.HasPrefix(v, "video"):
			return media.TrackVideo
		case strings.HasPrefix(v, "audio"):
			return media.TrackAudio
		}
	}
	if strings.Contains(strings.ToLower(rep.Codecs), "avc") || strings.Contains(strings.ToLower(rep.Codecs), "hvc") {
		return media.TrackVideo
	}
	return inferKind(rep.ID + as.ID)
}

func dashTrack(as mpdAdaptSet, rep mpdRepresent, kind media.TrackType) string {
	name := rep.ID
	if name == "" {
		name = as.ID
	}
	if kind == media.TrackVideo && !strings.Contains(strings.ToLower(name), "video") {
		name = "video-" + name
	}
	if kind == media.TrackAudio && !strings.Contains(strings.ToLower(name), "audio") {
		name = "audio-" + name
	}
	return trackIDFromName(name, "dash")
}

func dashEntries(tmpl *mpdTemplate, tl *mpdTimeline, timescale int64, kind media.TrackType, track, baseDir, repID string) ([]Entry, error) {
	if tmpl.Media == "" {
		return nil, fmt.Errorf("dash: representation %s has no media template", repID)
	}
	startNumber := int64(1)
	if tmpl.StartNumber != nil {
		startNumber = *tmpl.StartNumber
	}
	if startNumber < 0 {
		return nil, fmt.Errorf("dash: representation %s has negative startNumber", repID)
	}
	var out []Entry
	seq := int(startNumber)
	next := media.Ticks(tmpl.PresentationTO, timescale)
	add := func(start, dur media.Rational) error {
		uri, err := dashExpand(tmpl.Media, seq, start.Tick(timescale), repID, 0)
		if err != nil {
			return err
		}
		path, err := media.ResolveLocalURI(baseDir, uri)
		if err != nil {
			return err
		}
		out = append(out, Entry{
			Track:         track,
			Kind:          kind,
			Source:        "dash",
			Seq:           seq,
			URI:           uri,
			Path:          path,
			DeclaredStart: start,
			Duration:      dur,
			Timescale:     timescale,
		})
		seq++
		return nil
	}
	if tl != nil && len(tl.Segments) > 0 {
		for _, s := range tl.Segments {
			if s.D < 0 {
				return nil, fmt.Errorf("dash: negative segment duration %d", s.D)
			}
			dur := media.Ticks(s.D, timescale)
			if s.T != 0 {
				t := media.Ticks(s.T, timescale)
				if t.Cmp(next) > 0 {
					next = t
				}
			}
			repeats := s.R
			if repeats < 0 {
				repeats = 0 // r<0 means repeat until period end; unsupported locally
			}
			for r := int64(0); r <= repeats; r++ {
				if err := add(next, dur); err != nil {
					return nil, err
				}
				next = next.Add(dur)
			}
			if err := media.CheckTimelineLimit(media.Zero, next); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if tmpl.Duration <= 0 {
		return nil, fmt.Errorf("dash: representation %s has neither SegmentTimeline nor duration", repID)
	}
	dur := media.Ticks(tmpl.Duration, timescale)
	// Without an explicit end, emit one hour worth of segments at most.
	limit := media.NewRational(3600, 1)
	for next.Cmp(limit) < 0 {
		if err := add(next, dur); err != nil {
			return nil, err
		}
		next = next.Add(dur)
	}
	return out, nil
}

func dashExpand(tmpl string, number int, offset int64, repID string, bandwidth int64) (string, error) {
	if strings.ContainsRune(tmpl, 0) || strings.Contains(tmpl, "://") {
		return "", &media.FaultError{Kind: media.ErrUnsafeURI, Detail: tmpl}
	}
	out := tmpl
	for {
		start := strings.IndexByte(out, '$')
		if start < 0 {
			break
		}
		end := strings.IndexByte(out[start+1:], '$')
		if end < 0 {
			return "", fmt.Errorf("dash: unbalanced $ in media template %q", tmpl)
		}
		end += start + 1
		token := out[start+1 : end]
		repl, ok := expandToken(token, number, offset, repID, bandwidth)
		if !ok {
			return "", fmt.Errorf("dash: unsupported template token %q", token)
		}
		out = out[:start] + repl + out[end+1:]
	}
	return out, nil
}

func expandToken(token string, number int, offset int64, repID string, bandwidth int64) (string, bool) {
	name := token
	width := 0
	if idx := strings.IndexByte(token, '%'); idx >= 0 {
		name = token[:idx]
		w, err := strconv.Atoi(strings.TrimSuffix(token[idx+1:], "d"))
		if err != nil || w < 0 || w > 12 {
			return "", false
		}
		width = w
	}
	if name == "RepresentationID" {
		if repID == "" || strings.ContainsAny(repID, "/\\$") || repID == "." || repID == ".." {
			return "", false
		}
		return repID, true
	}
	var val int64
	switch name {
	case "Number":
		val = int64(number)
	case "Time":
		val = offset
	case "Bandwidth":
		val = bandwidth
	default:
		return "", false
	}
	s := strconv.FormatInt(val, 10)
	for len(s) < width {
		s = "0" + s
	}
	return s, true
}
