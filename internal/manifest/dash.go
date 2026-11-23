package manifest

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediarun/internal/clock"
)

// dashMPD is the minimal subset of an MPD needed to resolve a
// SegmentTimeline. Unknown elements are skipped by encoding/xml, so a richer
// document still parses.
type dashMPD struct {
	XMLName xml.Name     `xml:"MPD"`
	Type    string       `xml:"type,attr"`
	BaseURL string       `xml:"BaseURL"`
	Periods []dashPeriod `xml:"Period"`
}

type dashPeriod struct {
	ID       string              `xml:"id,attr"`
	BaseURL  string              `xml:"BaseURL"`
	AdaptSet []dashAdaptationSet `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	ID          string        `xml:"id,attr"`
	MimeType    string        `xml:"mimeType,attr"`
	ContentType string        `xml:"contentType,attr"`
	BaseURL     string        `xml:"BaseURL"`
	Timescale   uint64        `xml:"timescale,attr"`
	Reps        []dashRep     `xml:"Representation"`
	Timeline    *dashTimeline `xml:"SegmentTimeline"`
}

type dashRep struct {
	ID        string        `xml:"id,attr"`
	Bandwidth uint64        `xml:"bandwidth,attr"`
	BaseURL   string        `xml:"BaseURL"`
	Timeline  *dashTimeline `xml:"SegmentTimeline"`
	Template  *dashTemplate `xml:"SegmentTemplate"`
}

type dashTimeline struct {
	S []dashS `xml:"S"`
}

type dashS struct {
	T string `xml:"t,attr"`
	D string `xml:"d,attr"`
	R string `xml:"r,attr"`
	N string `xml:"n,attr"`
}

type dashTemplate struct {
	Media          string `xml:"media,attr"`
	Timescale      string `xml:"timescale,attr"`
	StartNumber    string `xml:"startNumber,attr"`
	Initialization string `xml:"initialization,attr"`
}

// ParseDASH parses a SegmentTimeline-bearing MPD into the neutral segment
// model. Every S entry expands into repeated segments; explicit t values must
// advance monotonically and zero/negative durations are rejected.
func ParseDASH(r io.Reader, limits Limits) (*Media, error) {
	if limits.MaxEntries <= 0 {
		limits = DefaultLimits()
	}
	dec := xml.NewDecoder(r)
	var doc dashMPD
	if err := dec.Decode(&doc); err != nil {
		return nil, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported, "", err)
	}
	if len(doc.Periods) == 0 {
		return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "", "MPD has no Period")
	}
	media := &Media{Kind: "dash"}
	entries := 0
	// Track labels must be unique across all AdaptationSets in all Periods:
	// two video sets otherwise share the "video" namespace and emit
	// colliding segment ids and URIs. The label is the declared id when
	// present, and a disambiguated fallback otherwise.
	used := map[string]int{}
	for pi, p := range doc.Periods {
		if len(p.AdaptSet) == 0 {
			return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported,
				fmt.Sprintf("period-%d", pi), "no AdaptationSet")
		}
		for ai, as := range p.AdaptSet {
			track := uniqueTrack(used, dashTrack(as), ai)
			ts := as.Timescale
			if ts == 0 {
				ts = 1000
			}
			if ts > 1_000_000_000 {
				return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration,
					track, "timescale too large")
			}
			tl := as.Timeline
			rep := pickRepresentation(as.Reps)
			if rep != nil && rep.Timeline != nil {
				tl = rep.Timeline
			}
			if rep != nil && rep.Template != nil && rep.Template.Timescale != "" {
				n, err := parseIntAttr(rep.Template.Timescale, track)
				if err != nil {
					return nil, err
				}
				if n <= 0 || n > 1_000_000_000 {
					return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration,
						track, "timescale out of range")
				}
				ts = uint64(n)
			}
			if tl == nil || len(tl.S) == 0 {
				return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported,
					track, "missing SegmentTimeline")
			}
			base := strings.TrimSpace(joinBase(doc.BaseURL, p.BaseURL, as.BaseURL))
			if rep != nil && rep.BaseURL != "" {
				base = joinBase(base, rep.BaseURL)
			}
			media.Timescale = ts
			// Trajectory always works in the segment timescale.
			tr := newTrajectory(ts)
			var cursor uint64
			num := uint64(0)
			if rep != nil && rep.Template != nil && rep.Template.StartNumber != "" {
				n, err := parseIntAttr(rep.Template.StartNumber, track)
				if err != nil {
					return nil, err
				}
				if n < 0 {
					return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration,
						track, "negative startNumber")
				}
				num = uint64(n)
			}
			hasT := false
			for _, s := range tl.S {
				d, err := parseUnsigned(s.D, track)
				if err != nil {
					return nil, err
				}
				if d == 0 {
					return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration,
						track, "zero segment duration")
				}
				repeat := uint64(0)
				if s.R != "" {
					rv, err := parseIntAttr(s.R, track)
					if err != nil {
						return nil, err
					}
					if rv < 0 || rv > int64(limits.MaxEntries) {
						return nil, clock.New(clock.KindInvalid, clock.CodeEntryLimit,
							track, "repeat count out of range")
					}
					repeat = uint64(rv)
				}
				t := cursor
				if s.T != "" {
					tv, err := parseUnsigned(s.T, track)
					if err != nil {
						return nil, err
					}
					if hasT && tv < cursor {
						return nil, clock.New(clock.KindInvalid, clock.CodeInvalidDuration,
							track, "explicit t moves backwards")
					}
					t = tv
					hasT = true
				} else if !hasT {
					hasT = true
				}
				cursor = t
				count := repeat + 1
				if entries+int(count) > limits.MaxEntries {
					return nil, clock.New(clock.KindInvalid, clock.CodeEntryLimit,
						track, "SegmentTimeline exceeds entry limit")
				}
				for k := uint64(0); k < count; k++ {
					durNS, err := ticksToNS(int64(d), ts, track)
					if err != nil {
						return nil, err
					}
					startNS, err := startToNS(cursor, ts, track)
					if err != nil {
						return nil, err
					}
					id := fmt.Sprintf("%s-%06d", track, num)
					uri := ""
					if rep != nil && rep.Template != nil {
						uri, err = expandMedia(rep.Template.Media, num)
						if err != nil {
							return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported,
								id, err.Error())
						}
						uri = joinBase(base, uri)
					} else {
						uri = joinBase(base, fmt.Sprintf("%s/%06d.m4s", track, num))
					}
					norm, err := normaliseURIPath(uri)
					if err != nil {
						return nil, clock.New(clock.KindInvalid, clock.CodePathEscape,
							uri, err.Error())
					}
					if err := tr.checkDuplicateHash(track, num, norm, ""); err != nil {
						return nil, err
					}
					media.Segments = append(media.Segments, Segment{
						ID:      id,
						Track:   track,
						Seq:     num,
						StartNS: startNS,
						DurNS:   durNS,
						URI:     norm,
					})
					entries++
					cursor += d
					num++
				}
			}
		}
	}
	if len(media.Segments) == 0 {
		return nil, clock.New(clock.KindInvalid, clock.CodeUnsupported, "", "no segments")
	}
	for _, s := range media.Segments {
		if s.DurNS > media.TargetDurNS {
			media.TargetDurNS = s.DurNS
		}
	}
	return media, nil
}

// uniqueTrack returns a label unique among all across the document. It keeps
// the declared id verbatim when that id is still free and otherwise appends a
// deterministic suffix, so segment ids never collide.
func uniqueTrack(used map[string]int, want string, index int) string {
	if want == "" || used[want] > 0 {
		want = want + "-" + strconv.Itoa(index)
	}
	used[want]++
	return want
}

func dashTrack(as dashAdaptationSet) string {
	ct := strings.ToLower(as.ContentType)
	mt := strings.ToLower(as.MimeType)
	switch {
	case strings.Contains(ct, "audio") || strings.Contains(mt, "audio"):
		return "audio"
	case strings.Contains(ct, "video") || strings.Contains(mt, "video"):
		return "video"
	case as.ID != "":
		return as.ID
	}
	return "video"
}

func pickRepresentation(reps []dashRep) *dashRep {
	if len(reps) == 0 {
		return nil
	}
	best := 0
	for i := range reps {
		if reps[i].Bandwidth > reps[best].Bandwidth {
			best = i
		}
	}
	return &reps[best]
}

// startToNS is ticksToNS for an axis position, which may legitimately be zero.
func startToNS(ticks uint64, timescale uint64, subject string) (int64, error) {
	if timescale == 0 {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			"zero timescale")
	}
	q := ticks / timescale
	if q > 9_000_000_000 {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			"start time too large")
	}
	r := ticks % timescale
	return int64(q)*1_000_000_000 + int64(r)*1_000_000_000/int64(timescale), nil
}

func parseUnsigned(s, subject string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := parseIntAttr(s, subject)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration, subject,
			"negative value")
	}
	return uint64(n), nil
}

func joinBase(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "/") {
			b.WriteString("/")
		}
		b.WriteString(p)
	}
	return b.String()
}

// expandMedia substitutes $Number$ (optionally formatted as $Number%06d$)
// and rejects unknown $...$ identifiers rather than guessing.
func expandMedia(tmpl string, number uint64) (string, error) {
	if tmpl == "" {
		return "", fmt.Errorf("empty media template")
	}
	var out strings.Builder
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '$' {
			out.WriteByte(tmpl[i])
			continue
		}
		j := strings.IndexByte(tmpl[i+1:], '$')
		if j < 0 {
			// A bare '$' with no closing delimiter is literal text per the
			// DASH template grammar, but two identifiers require both.
			return "", fmt.Errorf("unterminated template identifier")
		}
		end := i + 1 + j
		token := tmpl[i+1 : end]
		switch {
		case token == "Number":
			fmt.Fprintf(&out, "%d", number)
		case strings.HasPrefix(token, "Number%"):
			w, ok := parseFormatToken(token[7:])
			if !ok {
				return "", fmt.Errorf("malformed format token %q", token)
			}
			fmt.Fprintf(&out, "%0*d", w, number)
		default:
			return "", fmt.Errorf("unsupported template identifier %q", token)
		}
		i += j + 1
	}
	return out.String(), nil
}

// parseFormatToken parses the "05d"-style body of $Number%05d$ and returns
// the width, rejecting anything that is not digits followed by 'd'.
func parseFormatToken(s string) (int, bool) {
	if len(s) < 2 || s[len(s)-1] != 'd' {
		return 0, false
	}
	w := 0
	for _, c := range s[:len(s)-1] {
		if c < '0' || c > '9' {
			return 0, false
		}
		w = w*10 + int(c-'0')
		if w > 20 {
			return 0, false
		}
	}
	return w, true
}
