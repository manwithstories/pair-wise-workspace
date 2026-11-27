package manifest

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"

	"archive/internal/model"
)

// parseISO8601 parses the restricted xs:duration subset used by the local
// fixtures: PnDTnHnMnS with optional fractional seconds, e.g. PT4S, PT1.5S,
// PT1M30S. It returns seconds as a float.
func parseISO8601(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 3 || s[0] != 'P' {
		return 0, false
	}
	body := s[1:]
	divider := strings.IndexByte(body, 'T')
	datePart, timePart := body, ""
	if divider >= 0 {
		datePart, timePart = body[:divider], body[divider+1:]
	}
	var secs float64
	dateUnit := map[byte]float64{'Y': 0, 'M': 0, 'W': 0, 'D': 86400}
	timeUnit := map[byte]float64{'H': 3600, 'M': 60, 'S': 1}
	readPart := func(part string, units map[byte]float64, allowFloat bool) bool {
		for len(part) > 0 {
			i := 0
			for i < len(part) && (part[i] >= '0' && part[i] <= '9' || part[i] == '.') {
				i++
			}
			if i == 0 || i >= len(part) {
				return false
			}
			num, err := strconv.ParseFloat(part[:i], 64)
			if err != nil || (!allowFloat && strings.Contains(part[:i], ".")) {
				return false
			}
			mult, known := units[part[i]]
			if !known {
				return false
			}
			secs += num * mult
			part = part[i+1:]
		}
		return true
	}
	if !readPart(datePart, dateUnit, false) || !readPart(timePart, timeUnit, true) {
		return 0, false
	}
	return secs, secs > 0
}

var (
	attrRe    = regexp.MustCompile(`([A-Za-z_:][\w:.-]*)\s*=\s*"([^"]*)"`)
	tagOpenRe = regexp.MustCompile(`^[A-Za-z_][\w:.-]*`)
)

// scanTags emits raw XML elements as (name, attrs, selfClosing) tuples.
// It is a deliberately small scanner for the controlled local MPD fixtures,
// not a general XML processor.
type tag struct {
	name   string
	attrs  map[string]string
	self   bool
}

func scanTags(r io.Reader) ([]tag, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var tags []tag
	for scanner.Scan() {
		line := scanner.Text()
		for {
			open := strings.IndexByte(line, '<')
			if open < 0 {
				break
			}
			close := strings.IndexByte(line[open:], '>')
			if close < 0 {
				break
			}
			raw := line[open+1 : open+close]
			line = line[open+close+1:]
			if raw == "" || raw[0] == '?' || raw[0] == '!' || raw[0] == '/' {
				continue
			}
			name := tagOpenRe.FindString(raw)
			if name == "" {
				continue
			}
			attrs := map[string]string{}
			for _, m := range attrRe.FindAllStringSubmatch(raw, -1) {
				attrs[m[1]] = m[2]
			}
			tags = append(tags, tag{name: name, attrs: attrs, self: strings.HasSuffix(raw, "/")})
		}
	}
	return tags, scanner.Err()
}

// ParseDASH reads a local DASH MPD (supplied as r; base is the directory of
// the MPD file). SegmentTemplate with $Number$ expansion and explicit
// SegmentList/S entries are supported. Remote BaseURL values are rejected.
func ParseDASH(r io.Reader, base string, log *model.FaultLog) (*Timeline, error) {
	tags, err := scanTags(r)
	if err != nil {
		return nil, model.InvariantError(model.PhaseManifest, "read error: %v", err)
	}
	b := newBuilder(FormatDASH, base, log)

	var (
		startNum                     = 1
		tmplMedia, timescale         string
		tmplDur, periodDur, segDur   float64
		haveTmplDur, haveSegDur      bool
		pendingDiscont               bool
	)
	resolveNumber := func(n int) (string, bool) {
		if tmplMedia == "" {
			return "", false
		}
		return strings.ReplaceAll(tmplMedia, "$Number$", strconv.Itoa(n)), true
	}

	for _, t := range tags {
		switch t.name {
		case "BaseURL":
			if v := t.attrs["sourceURL"]; strings.Contains(v, "://") {
				log.Add(model.Fault{Phase: model.PhaseManifest, Kind: model.FaultPathRejected,
					Detail: "remote BaseURL is not allowed: " + v})
				return nil, model.InvariantError(model.PhaseManifest, "remote BaseURL is not allowed")
			}
		case "SegmentTemplate":
			tmplMedia = t.attrs["media"]
			timescale = t.attrs["timescale"]
			startNum = atoiDefault(t.attrs["startNumber"], 1)
			if d := t.attrs["duration"]; d != "" {
				if v, err := strconv.ParseFloat(d, 64); err == nil && v > 0 {
					tmplDur, haveTmplDur = v, true
				}
			}
		case "SegmentList":
			if v := t.attrs["timescale"]; v != "" {
				timescale = v
			}
			if d := t.attrs["duration"]; d != "" {
				if v, err := strconv.ParseFloat(d, 64); err == nil && v > 0 {
					segDur, haveSegDur = v, true
				}
			}
		case "SegmentURL", "Segment":
			uri := firstAttr(t.attrs, "media", "sourceURL")
			durAttr := firstAttr(t.attrs, "duration", "d")
			dur := 0.0
			if durAttr != "" {
				if v, err := strconv.ParseFloat(durAttr, 64); err == nil {
					dur = v
				}
			}
			if dur == 0 && haveSegDur {
				dur = segDur
			}
			if t.attrs["discontinuity"] == "true" {
				pendingDiscont = true
			}
			idx := len(b.tl.Segments)
			if err := b.add(startNum+idx, uri, normaliseDASHSecs(dur, timescale), pendingDiscont); err != nil {
				return nil, err
			}
			pendingDiscont = false
		case "S":
			d := t.attrs["d"]
			if d != "" {
				if v, err := strconv.ParseFloat(d, 64); err == nil && v > 0 {
					segDur, haveSegDur = v, true
				}
			}
		case "Period":
			if v := t.attrs["duration"]; v != "" {
				if secs, ok := parseISO8601(v); ok {
					periodDur = secs
				}
			}
		}
	}

	// SegmentTemplate with no explicit Segment list: expand $Number$ over
	// the Period duration using the template's fixed segment duration.
	if len(b.tl.Segments) == 0 && tmplMedia != "" && haveTmplDur {
		dur := normaliseDASHSecs(tmplDur, timescale)
		count := int(periodDur/dur + 0.5)
		if count <= 0 {
			return nil, model.InvariantError(model.PhaseManifest, "MPD yields no segments")
		}
		if count > model.MaxSegments {
			return nil, model.InvariantError(model.PhaseManifest, "segment count exceeds limit")
		}
		for i := 0; i < count; i++ {
			uri, ok := resolveNumber(startNum + i)
			if !ok {
				return nil, model.InvariantError(model.PhaseManifest, "template has no media pattern")
			}
			if err := b.add(startNum+i, uri, dur, false); err != nil {
				return nil, err
			}
		}
	}
	if len(b.tl.Segments) == 0 {
		return nil, model.InvariantError(model.PhaseManifest, "MPD contains no segments")
	}
	return b.tl, nil
}

func normaliseDASHSecs(v float64, timescale string) float64 {
	ts := atoiDefault(timescale, 0)
	if ts > 0 {
		return v / float64(ts)
	}
	return v
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func firstAttr(attrs map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := attrs[k]; ok && v != "" {
			return v
		}
	}
	return ""
}
