package subtitle

import (
	"strings"

	"mediascan/internal/media"
)

// ParseVTT parses WebVTT text, preserving cue identifiers, cue settings and
// the "WEBVTT" header so a round trip is byte-faithful for well-formed input.
func ParseVTT(data []byte, limits Limits) (*Document, error) {
	limits = limits.normalize()
	if int64(len(data)) > limits.MaxBytes {
		return nil, media.NewFatal(media.ClassSubtitleOversizedInput,
			media.Locator{ByteLength: int64(len(data))},
			"VTT input is %d bytes, limit is %d", len(data), limits.MaxBytes)
	}
	doc := &Document{Format: FormatVTT, SourceSize: int64(len(data))}
	lines, trailing := splitLinesNormalized(data)
	doc.TrailingNewline = trailing

	i := 0
	// Header: a WEBVTT magic line, an optional text block, then a blank line.
	if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i].text), "WEBVTT") {
		hdrStart := i
		i++
		for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
			i++
		}
		var b strings.Builder
		for k := hdrStart; k < i; k++ {
			b.WriteString(lines[k].text)
			b.WriteByte('\n')
		}
		doc.Header = b.String()
	} else {
		doc.Issues.Add(media.NewIssue(media.ClassSubtitleBadTimestamp, media.Locator{},
			"missing WEBVTT header"))
	}

	position := 0
	for i < len(lines) {
		for i < len(lines) && strings.TrimSpace(lines[i].text) == "" {
			i++
		}
		if i >= len(lines) {
			break
		}
		// NOTE block: "NOTE ..." until blank; preserved as an issue-free skip.
		if strings.HasPrefix(strings.TrimSpace(lines[i].text), "NOTE") {
			for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
				i++
			}
			continue
		}
		position++
		if len(doc.Cues) >= limits.MaxCues {
			doc.Issues.Add(media.NewFatal(media.ClassSubtitleTooManyCues,
				media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
				"cue count exceeds limit %d", limits.MaxCues))
			return doc, nil
		}
		blockStart := lines[i].offset

		// The timing line may be preceded by an identifier line.
		identifier := ""
		if !strings.Contains(lines[i].text, "-->") {
			if i+1 >= len(lines) || !strings.Contains(lines[i+1].text, "-->") {
				doc.Issues.Add(media.NewIssue(media.ClassSubtitleBadTimestamp,
					media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
					"expected cue timing line, found %q", truncateForMessage(lines[i].text)))
				i++
				continue
			}
			identifier = lines[i].text
			i++
		}

		cue, issue, consumed, ok := parseVTTCue(lines, i, position, identifier)
		if !ok {
			doc.Issues.Add(media.NewIssue(media.ClassSubtitleBadTimestamp,
				media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
				"malformed timing line %q", truncateForMessage(lines[i].text)))
			i++
			continue
		}
		if issue != nil {
			doc.Issues.Add(*issue)
		}
		cue.LineOffset = blockStart
		doc.Cues = append(doc.Cues, cue)
		i = consumed
	}
	return doc, nil
}

func parseVTTCue(lines []line, i, position int, identifier string) (Cue, *media.Issue, int, bool) {
	startRaw, endRaw, settings, ok := splitVTTTimingLine(lines[i].text)
	if !ok {
		return Cue{}, nil, i, false
	}
	startNs, ok1 := ParseTimestamp(startRaw)
	endNs, ok2 := ParseTimestamp(endRaw)
	if !ok1 || !ok2 {
		is := media.NewIssue(media.ClassSubtitleBadTimestamp,
			media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
			"malformed timing line %q", truncateForMessage(lines[i].text))
		return Cue{}, &is, i + 1, true
	}
	j := i + 1
	var text strings.Builder
	for j < len(lines) && strings.TrimSpace(lines[j].text) != "" {
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(lines[j].text)
		j++
	}
	cue := Cue{
		Index: position, StartNs: startNs, EndNs: endNs,
		StartRaw: startRaw, EndRaw: endRaw, Text: text.String(),
		Identifier: identifier, Settings: settings,
	}
	if j >= len(lines) && text.Len() == 0 {
		is := media.NewIssue(media.ClassSubtitleTruncatedCue,
			media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset), TimeNs: startNs},
			"cue has no text and no terminating newline (truncated tail)")
		return cue, &is, j, true
	}
	if endNs < startNs {
		is := media.NewIssue(media.ClassSubtitleNegativeRange,
			media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset), TimeNs: startNs},
			"negative duration: end %q precedes start %q", endRaw, startRaw)
		cue.EndNs = cue.StartNs
		return cue, &is, j, true
	}
	return cue, nil, j, true
}

// splitVTTTimingLine splits "start --> end settings...".
func splitVTTTimingLine(s string) (start, end, settings string, ok bool) {
	idx := strings.Index(s, "-->")
	if idx < 0 {
		return "", "", "", false
	}
	start = strings.TrimSpace(s[:idx])
	rest := strings.TrimSpace(s[idx+3:])
	if rest == "" {
		return "", "", "", false
	}
	end = rest
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		end = strings.TrimSpace(rest[:sp])
		settings = strings.TrimSpace(rest[sp:])
	}
	if start == "" || end == "" {
		return "", "", "", false
	}
	return start, end, settings, true
}
