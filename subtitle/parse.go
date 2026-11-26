// Package subtitle provides tolerant SRT/WebVTT parsing and astisub-backed
// lossless round trips for valid cues.
package subtitle

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"medcheck/fault"
)

// Format selects SRT or WebVTT.
type Format string

const (
	FormatSRT Format = "srt"
	FormatVTT Format = "vtt"
	// MaxCueBytes is the enforced maximum size of one cue's text.
	MaxCueBytes int64 = 1 << 20
)

var ErrCueTooLarge = errors.New("subtitle: cue exceeds 1 MiB")

// Cue is source-line aware and uses millisecond boundaries.
type Cue struct {
	SourceLine int    `json:"source_line"`
	Identifier string `json:"identifier,omitempty"`
	StartMS    int64  `json:"start_ms"`
	EndMS      int64  `json:"end_ms"`
	Text       string `json:"text"`
}

// Issue identifies an isolated malformed, overlapping or truncated source block.
type Issue struct {
	SourceLine int        `json:"source_line"`
	StartMS    int64      `json:"start_ms,omitempty"`
	EndMS      int64      `json:"end_ms,omitempty"`
	Kind       fault.Kind `json:"kind"`
	Message    string     `json:"message"`
	Raw        string     `json:"raw,omitempty"`
}

// Document contains recoverable cues and source-localized issues.
type Document struct {
	Format Format  `json:"format"`
	Cues   []Cue   `json:"cues"`
	Issues []Issue `json:"issues"`
}

// GetCues safely returns cues for a possibly nil document.
func (d *Document) GetCues() []Cue {
	if d == nil {
		return nil
	}
	return d.Cues
}

type locatedLine struct {
	num  int
	text string
}

// Parse tolerantly parses SRT or WebVTT while isolating bad source blocks.
func Parse(r io.Reader, format Format) (*Document, error) {
	doc := &Document{Format: format}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), int(MaxCueBytes)+4096)
	var block []locatedLine
	flush := func() {
		if len(block) == 0 {
			return
		}
		if cue, issue := processBlock(block); issue != nil {
			doc.Issues = append(doc.Issues, *issue)
		} else {
			doc.Cues = append(doc.Cues, cue)
		}
		block = nil
	}

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if lineNo == 1 && format == FormatVTT && strings.HasPrefix(strings.TrimSpace(line), "WEBVTT") {
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		block = append(block, locatedLine{num: lineNo, text: line})
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("subtitle: source line around %d too long: %w", lineNo, ErrCueTooLarge)
		}
		return nil, err
	}
	flush()
	detectOverlaps(doc)
	return doc, nil
}

func processBlock(block []locatedLine) (Cue, *Issue) {
	timingIdx := -1
	for i, line := range block {
		if strings.Contains(line.text, "-->") {
			timingIdx = i
			break
		}
	}
	first := block[0]
	if timingIdx < 0 {
		joined := joinRaw(block)
		kind := fault.MalformedCue
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(first.text)), "NOTE") ||
			strings.HasPrefix(strings.TrimSpace(first.text), "STYLE") ||
			strings.HasPrefix(strings.TrimSpace(first.text), "REGION") {
			return Cue{}, nil
		}
		return Cue{}, &Issue{SourceLine: first.num, Kind: kind, Message: "missing cue timing", Raw: joined}
	}

	timing := block[timingIdx].text
	parts := strings.SplitN(timing, "-->", 2)
	start, err1 := parseTimestamp(strings.TrimSpace(parts[0]))
	endFields := strings.Fields(strings.TrimSpace(parts[1]))
	var end int64
	var err2 error
	if len(endFields) == 0 {
		err2 = errors.New("missing end timestamp")
	} else {
		end, err2 = parseTimestamp(endFields[0])
	}
	if err1 != nil || err2 != nil {
		return Cue{}, &Issue{SourceLine: block[timingIdx].num, Kind: fault.MalformedCue,
			Message: fmt.Sprintf("bad timing: %v / %v", err1, err2), Raw: timing}
	}
	if end <= start {
		return Cue{}, &Issue{SourceLine: block[timingIdx].num, Kind: fault.MalformedCue,
			Message: "cue end must be after start", Raw: timing}
	}

	var id string
	if timingIdx > 0 {
		id = strings.TrimSpace(block[0].text)
	}
	textLines := block[timingIdx+1:]
	var textParts []string
	var total int64
	for _, line := range textLines {
		total += int64(len(line.text)) + 1
		textParts = append(textParts, strings.TrimSpace(line.text))
	}
	text := strings.TrimSpace(strings.Join(textParts, "\n"))
	if text == "" {
		return Cue{}, &Issue{SourceLine: block[timingIdx].num, StartMS: start, EndMS: end,
			Kind: fault.TruncatedCue, Message: "cue has timing but no text", Raw: timing}
	}
	if total > MaxCueBytes || int64(len(text)) > MaxCueBytes {
		return Cue{}, &Issue{SourceLine: block[timingIdx].num, Kind: fault.MalformedCue,
			Message: ErrCueTooLarge.Error(), Raw: timing}
	}
	return Cue{SourceLine: block[timingIdx].num, Identifier: id, StartMS: start, EndMS: end, Text: text}, nil
}

func joinRaw(block []locatedLine) string {
	parts := make([]string, len(block))
	for i, l := range block {
		parts[i] = l.text
	}
	return strings.Join(parts, "\n")
}

func parseTimestamp(value string) (int64, error) {
	value = strings.TrimSpace(value)
	value = strings.Replace(value, ",", ".", 1)
	colon := strings.Split(value, ":")
	var hours, minutes int
	var rest string
	switch len(colon) {
	case 2:
		minutes = mustAtoi(colon[0])
		rest = colon[1]
	case 3:
		hours = mustAtoi(colon[0])
		minutes = mustAtoi(colon[1])
		rest = colon[2]
	default:
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	secMS := strings.SplitN(rest, ".", 2)
	seconds := mustAtoi(secMS[0])
	ms := 0
	if len(secMS) == 2 {
		fraction := secMS[1]
		for len(fraction) < 3 {
			fraction += "0"
		}
		if len(fraction) > 3 {
			fraction = fraction[:3]
		}
		ms = mustAtoi(fraction)
	}
	if minutes > 59 || seconds > 59 {
		return 0, fmt.Errorf("timestamp field out of range %q", value)
	}
	return int64(hours*3600000 + minutes*60000 + seconds*1000 + ms), nil
}

func mustAtoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func detectOverlaps(doc *Document) {
	for i := 1; i < len(doc.Cues); i++ {
		prev, cur := doc.Cues[i-1], doc.Cues[i]
		if cur.StartMS < prev.EndMS {
			doc.Issues = append(doc.Issues, Issue{SourceLine: cur.SourceLine, StartMS: cur.StartMS, EndMS: cur.EndMS,
				Kind: fault.OverlappingCue, Message: fmt.Sprintf("overlaps cue at source line %d", prev.SourceLine)})
		}
	}
}
