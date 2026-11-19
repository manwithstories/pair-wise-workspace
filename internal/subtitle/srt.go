package subtitle

import (
	"strings"

	"pairflow/media/internal/clock"
)

// ParseSRT parses SubRip data. It tolerates a UTF-8 BOM, CRLF and LF
// line endings, missing sequence numbers and legal overlaps. A record
// whose timing line is malformed is reported with its raw byte offset
// and skipped; a final record truncated mid-text is kept when its
// timing line was complete, and flagged otherwise.
func ParseSRT(data []byte, track string) *Document {
	doc := &Document{Format: "srt", Track: track}
	lines, bom, crlf := splitLines(data)
	doc.BOM, doc.CRLF = bom, crlf

	i := 0
	for i < len(lines) {
		// Skip blank separators.
		for i < len(lines) && strings.TrimSpace(lines[i].text) == "" {
			i++
		}
		if i >= len(lines) {
			break
		}
		recStart := i
		startLine := lines[i]
		seq := ""
		timing := ""
		if strings.Contains(startLine.text, "-->") {
			timing = startLine.text
		} else {
			seq = strings.TrimSpace(startLine.text)
			if i+1 < len(lines) && strings.Contains(lines[i+1].text, "-->") {
				timing = lines[i+1].text
				i++
			} else {
				se := clock.Errorf("srt", "missing_timing", startLine.offset,
					"record at line %d has no timestamp line", recStart+1)
				se.Raw = startLine.text
				doc.Errors = append(doc.Errors, se)
				i = skipRecord(lines, recStart)
				continue
			}
		}

		start, end, ok := parseArrow(timing)
		if !ok {
			se := clock.Errorf("srt", "bad_timestamp", startLine.offset,
				"cannot parse timing line")
			se.Raw = timing
			doc.Errors = append(doc.Errors, se)
			i = skipRecord(lines, i)
			continue
		}
		if end.LT(start) {
			se := clock.Errorf("srt", "negative_duration", startLine.offset,
				"end precedes start")
			se.Raw = timing
			doc.Errors = append(doc.Errors, se)
			i = skipRecord(lines, i)
			continue
		}

		// Collect text lines until a blank line or the next record.
		i++
		var text []line
		for i < len(lines) {
			if strings.TrimSpace(lines[i].text) == "" {
				break
			}
			if strings.Contains(lines[i].text, "-->") && len(text) > 0 {
				break
			}
			text = append(text, lines[i])
			i++
		}
		cue := Cue{
			Track:    track,
			Format:   "srt",
			Start:    start,
			End:      end,
			Offset:   startLine.offset,
			EndOff:   endRecordOffset(text, startLine.offset),
			Sequence: seq,
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
			se := clock.Errorf("srt", "truncated_text", text[len(text)-1].offset,
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

// parseArrow splits a "start --> end [settings]" timing line.
func parseArrow(s string) (clock.Rational, clock.Rational, bool) {
	idx := strings.Index(s, "-->")
	if idx < 0 {
		return clock.Zero(), clock.Zero(), false
	}
	left := strings.TrimSpace(s[:idx])
	right := strings.TrimSpace(s[idx+3:])
	// Drop SRT position settings after the end timestamp.
	if f := strings.Fields(right); len(f) > 0 {
		right = f[0]
	}
	start, ok1 := timestamp(left)
	end, ok2 := timestamp(right)
	return start, end, ok1 && ok2
}

// endRecordOffset returns the byte offset just past the last line of a
// record.
func endRecordOffset(text []line, fallback int64) int64 {
	if len(text) == 0 {
		return fallback
	}
	last := text[len(text)-1]
	return last.offset + int64(len(last.text))
}

// skipRecord advances past a malformed record.
func skipRecord(lines []line, i int) int {
	if i < len(lines) {
		i++
	}
	for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
		i++
	}
	return i
}
