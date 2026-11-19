package subtitle

import (
	"strconv"
	"strings"

	"pairflow/media/internal/clock"
)

// ParseVTT parses WebVTT. It accepts the WEBVTT signature, optional
// NOTE/STYLE/REGION blocks, cue identifiers, cue settings and both
// timestamp separators. The same recovery policy as SRT applies.
func ParseVTT(data []byte, track string) *Document {
	doc := &Document{Format: "vtt", Track: track}
	lines, bom, crlf := splitLines(data)
	doc.BOM, doc.CRLF = bom, crlf
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0].text), "WEBVTT") {
		se := clock.NewSourceError("vtt", "missing_signature", 0, "missing WEBVTT signature")
		doc.Errors = append(doc.Errors, se)
		// Continue: a missing signature is recoverable for inspection.
	}

	i := 0
	// Skip the signature and its header block.
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0].text), "WEBVTT") {
		i = 1
		for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
			i++
		}
	}

	for i < len(lines) {
		for i < len(lines) && strings.TrimSpace(lines[i].text) == "" {
			i++
		}
		if i >= len(lines) {
			break
		}
		head := lines[i].text
		trimmed := strings.TrimSpace(head)
		if strings.HasPrefix(trimmed, "NOTE") || strings.HasPrefix(trimmed, "STYLE") || strings.HasPrefix(trimmed, "REGION") {
			for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
				i++
			}
			continue
		}
		recStart := i
		identifier := ""
		timing := ""
		if strings.Contains(head, "-->") {
			timing = head
		} else if i+1 < len(lines) && strings.Contains(lines[i+1].text, "-->") {
			identifier = trimmed
			timing = lines[i+1].text
			i++
		} else {
			se := clock.Errorf("vtt", "missing_timing", lines[recStart].offset,
				"cue block has no timing line")
			se.Raw = head
			doc.Errors = append(doc.Errors, se)
			i = skipRecord(lines, recStart)
			continue
		}

		start, end, settings, ok := parseVTTTiming(timing)
		if !ok {
			se := clock.Errorf("vtt", "bad_timestamp", lines[recStart].offset,
				"cannot parse cue timing")
			se.Raw = timing
			doc.Errors = append(doc.Errors, se)
			i = skipRecord(lines, i)
			continue
		}
		if end.LT(start) {
			se := clock.Errorf("vtt", "negative_duration", lines[recStart].offset,
				"cue end precedes start")
			se.Raw = timing
			doc.Errors = append(doc.Errors, se)
			i = skipRecord(lines, i)
			continue
		}

		i++
		var text []line
		for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
			text = append(text, lines[i])
			i++
		}
		cue := Cue{
			Track:    track,
			Format:   "vtt",
			Start:    start,
			End:      end,
			Offset:   lines[recStart].offset,
			EndOff:   endRecordOffset(text, lines[recStart].offset),
			Sequence: identifier,
			Settings: settings,
		}
		var b strings.Builder
		for k, l := range text {
			if k > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(l.text)
		}
		cue.Text = b.String()
		if len(text) > 0 && text[len(text)-1].truncated {
			doc.Truncated = true
			se := clock.Errorf("vtt", "truncated_text", text[len(text)-1].offset,
				"final cue text has no terminating newline")
			se.Raw = text[len(text)-1].text
			doc.Errors = append(doc.Errors, se)
		}
		doc.Cues = append(doc.Cues, cue)
	}
	for i := range doc.Cues {
		doc.Cues[i].Index = i
	}
	return doc
}

func parseVTTTiming(s string) (clock.Rational, clock.Rational, string, bool) {
	idx := strings.Index(s, "-->")
	if idx < 0 {
		return clock.Zero(), clock.Zero(), "", false
	}
	left := strings.TrimSpace(s[:idx])
	right := strings.TrimSpace(s[idx+3:])
	settings := ""
	if f := strings.Fields(right); len(f) > 0 {
		right = f[0]
		settings = strings.Join(f[1:], " ")
	}
	start, ok1 := timestamp(left)
	end, ok2 := timestamp(right)
	return start, end, settings, ok1 && ok2
}

// FormatSeconds renders a rational time for fixture generation as
// SRT-style hh:mm:ss,mmm.
func FormatSeconds(r clock.Rational) string {
	ms := r.Millis()
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	sec := ms / 1000
	ms -= sec * 1000
	var b strings.Builder
	b.WriteString(pad2(h))
	b.WriteByte(':')
	b.WriteString(pad2(m))
	b.WriteByte(':')
	b.WriteString(pad2(sec))
	b.WriteByte(',')
	b.WriteString(pad3(ms))
	return b.String()
}

// FormatVTTSeconds renders a rational time as VTT hh:mm:ss.mmm.
func FormatVTTSeconds(r clock.Rational) string {
	return strings.Replace(FormatSeconds(r), ",", ".", 1)
}

func pad2(v int64) string {
	s := strconv.FormatInt(v, 10)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

func pad3(v int64) string {
	s := strconv.FormatInt(v, 10)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}
