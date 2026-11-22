package subtitle

import (
	"strings"

	"mediascan/internal/media"
)

// ParseSRT parses SubRip text. Structural defects are returned as issues
// rather than silently ignored: an invalid sequence number, a negative or
// reversed cue duration, and a truncated final cue each produce their own
// issue class. A file that cannot be parsed at all returns an error.
func ParseSRT(data []byte, limits Limits) (*Document, error) {
	limits = limits.normalize()
	if int64(len(data)) > limits.MaxBytes {
		return nil, media.NewFatal(media.ClassSubtitleOversizedInput,
			media.Locator{ByteLength: int64(len(data))},
			"SRT input is %d bytes, limit is %d", len(data), limits.MaxBytes)
	}
	doc := &Document{Format: FormatSRT, SourceSize: int64(len(data))}
	lines, trailing := splitLinesNormalized(data)
	doc.TrailingNewline = trailing

	i := 0
	position := 0 // 1-based cue position for fallback indexing
	for i < len(lines) {
		// Skip blank runs between blocks.
		for i < len(lines) && strings.TrimSpace(lines[i].text) == "" {
			i++
		}
		if i >= len(lines) {
			break
		}
		position++
		if len(doc.Cues) >= limits.MaxCues {
			doc.Issues.Add(media.NewFatal(media.ClassSubtitleTooManyCues,
				media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
				"cue count exceeds limit %d", limits.MaxCues))
			return doc, nil
		}
		blockStart := lines[i].offset

		// Optional sequence line: a single integer.
		index := position
		seqLine := -1
		if i < len(lines) && isAllDigits(strings.TrimSpace(lines[i].text)) {
			seqLine = i
			n, err := parseInt(strings.TrimSpace(lines[i].text))
			if err != nil || n <= 0 {
				doc.Issues.Add(media.NewIssue(media.ClassSubtitleInvalidIndex,
					media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
					"invalid sequence number %q", strings.TrimSpace(lines[i].text)))
			} else {
				index = int(n)
				if index != position {
					doc.Issues.Add(media.NewIssue(media.ClassSubtitleInvalidIndex,
						media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
						"sequence number %d is out of order (expected %d)", index, position))
				}
			}
			i++
		}

		if i >= len(lines) {
			doc.Issues.Add(media.NewIssue(media.ClassSubtitleTruncatedCue,
				media.Locator{CueIndex: position, ByteOffset: int64(blockStart)},
				"cue header ends at EOF before a timing line"))
			break
		}

		cue, issue, consumed, parsed := parseSRTCue(lines, i, position, index)
		if !parsed {
			// Not a timing line: the block is malformed. Skip one line and
			// continue so a single bad cue does not fail the file.
			doc.Issues.Add(media.NewIssue(media.ClassSubtitleBadTimestamp,
				media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
				"expected timing line, found %q", truncateForMessage(lines[i].text)))
			i++
			continue
		}
		if issue != nil {
			doc.Issues.Add(*issue)
		}
		if cue != nil {
			cue.LineOffset = blockStart
			doc.Cues = append(doc.Cues, *cue)
		}
		i = consumed
		_ = seqLine
	}
	return doc, nil
}

// parseSRTCue reads a timing line at lines[i] plus its text body. It returns
// the cue (nil when the timing line was malformed and reported as an issue),
// the consumed index (exclusive), and whether a timing line was recognized.
func parseSRTCue(lines []line, i, position, index int) (*Cue, *media.Issue, int, bool) {
	startRaw, endRaw, ok := splitTimingLine(lines[i].text)
	if !ok {
		return nil, nil, i, false
	}
	startNs, ok1 := ParseTimestamp(startRaw)
	endNs, ok2 := ParseTimestamp(endRaw)
	if !ok1 || !ok2 {
		is := media.NewIssue(media.ClassSubtitleBadTimestamp,
			media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset)},
			"malformed timing line %q", truncateForMessage(lines[i].text))
		return nil, &is, i + 1, true
	}
	// Collect text until the next blank line.
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
		Index: index, StartNs: startNs, EndNs: endNs,
		StartRaw: startRaw, EndRaw: endRaw, Text: text.String(),
	}
	// A cue at EOF with no terminating blank line is still complete if it has
	// text; a timing line with no body at EOF is truncated.
	if j >= len(lines) && text.Len() == 0 {
		is := media.NewIssue(media.ClassSubtitleTruncatedCue,
			media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset), TimeNs: startNs},
			"cue has no text and no terminating newline (truncated tail)")
		return &cue, &is, j, true
	}
	if endNs < startNs {
		is := media.NewIssue(media.ClassSubtitleNegativeRange,
			media.Locator{CueIndex: position, ByteOffset: int64(lines[i].offset), TimeNs: startNs},
			"negative duration: end %q precedes start %q", endRaw, startRaw)
		cue.EndNs = cue.StartNs // normalize so downstream never sees a negative span
		return &cue, &is, j, true
	}
	return &cue, nil, j, true
}

// splitTimingLine splits "00:00:01,000 --> 00:00:02,000" on "-->".
func splitTimingLine(s string) (start, end string, ok bool) {
	idx := strings.Index(s, "-->")
	if idx < 0 {
		return "", "", false
	}
	start = strings.TrimSpace(s[:idx])
	rest := strings.TrimSpace(s[idx+3:])
	// SRT has no cue settings, but tolerate trailing coordinates.
	end = rest
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		end = strings.TrimSpace(rest[:sp])
	}
	if start == "" || end == "" {
		return "", "", false
	}
	return start, end, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func truncateForMessage(s string) string {
	const max = 48
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
