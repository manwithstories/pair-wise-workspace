// Package subtitle parses SRT and WebVTT files tolerating BOM and CRLF,
// isolating malformed cues (recorded with their byte offset) and preserving
// every valid cue before a truncated tail. Cues can be cropped onto the
// unified timeline for per-segment slicing.
package subtitle

import (
	"bytes"
	"strconv"
	"strings"

	"presspack/internal/core"
)

// Cue is one subtitle event on the unified timeline.
type Cue struct {
	Start, End core.Time
	Text       string
}

// line is a physical line with its byte offset in the original input.
type line struct {
	off  int64
	text string
}

// splitLines normalizes CRLF/CR to LF and returns lines with offsets.
// A UTF-8 BOM is stripped but counted in offsets.
func splitLines(data []byte) []line {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	raw := strings.Split(string(data), "\n")
	lines := make([]line, 0, len(raw))
	var off int64
	for i, l := range raw {
		if i == 0 {
			l = strings.TrimPrefix(l, "\uFEFF")
		}
		lines = append(lines, line{off: off, text: l})
		off += int64(len(l)) + 1 // offsets refer to normalized stream
	}
	return lines
}

// block groups the lines of one cue (blank-line separated).
type block struct {
	off   int64
	lines []string
}

func groupBlocks(lines []line) []block {
	var blocks []block
	var cur *block
	for _, l := range lines {
		if strings.TrimSpace(l.text) == "" {
			cur = nil
			continue
		}
		if cur == nil {
			blocks = append(blocks, block{off: l.off})
			cur = &blocks[len(blocks)-1]
		}
		cur.lines = append(cur.lines, l.text)
	}
	return blocks
}

// ParseTimestamp reads "HH:MM:SS,mmm", "HH:MM:SS.mmm" or "MM:SS.mmm".
func ParseTimestamp(s string) (core.Time, error) {
	s = strings.TrimSpace(s)
	s = strings.Replace(s, ",", ".", 1)
	var h, m, sec, ms int64
	parts := strings.Split(s, ":")
	var secPart string
	switch len(parts) {
	case 2:
		secPart = parts[1]
		if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
			return core.Time{}, errInvalidTimestamp(s)
		}
		m, _ = strconv.ParseInt(parts[0], 10, 64)
	case 3:
		var err1, err2 error
		h, err1 = strconv.ParseInt(parts[0], 10, 64)
		m, err2 = strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil {
			return core.Time{}, errInvalidTimestamp(s)
		}
		secPart = parts[2]
	default:
		return core.Time{}, errInvalidTimestamp(s)
	}
	sp := strings.SplitN(secPart, ".", 2)
	if len(sp) != 2 || len(sp[1]) != 3 {
		return core.Time{}, errInvalidTimestamp(s)
	}
	var err error
	if sec, err = strconv.ParseInt(sp[0], 10, 64); err != nil {
		return core.Time{}, errInvalidTimestamp(s)
	}
	if ms, err = strconv.ParseInt(sp[1], 10, 64); err != nil {
		return core.Time{}, errInvalidTimestamp(s)
	}
	if m > 59 || sec > 59 {
		return core.Time{}, errInvalidTimestamp(s)
	}
	return core.FromMillis((h*3600+m*60+sec)*1000 + ms), nil
}

type tsError string

func (e tsError) Error() string          { return "invalid timestamp: " + string(e) }
func errInvalidTimestamp(s string) error { return tsError(s) }

// parseTimeline extracts "start --> end" from a cue timing line. VTT
// settings after the end timestamp are ignored.
func parseTimeline(s string) (core.Time, core.Time, error) {
	i := strings.Index(s, "-->")
	if i < 0 {
		return core.Time{}, core.Time{}, errInvalidTimestamp(s)
	}
	start, err := ParseTimestamp(s[:i])
	if err != nil {
		return core.Time{}, core.Time{}, err
	}
	rest := strings.TrimSpace(s[i+3:])
	if j := strings.IndexAny(rest, " \t"); j >= 0 {
		rest = rest[:j]
	}
	end, err := ParseTimestamp(rest)
	if err != nil {
		return core.Time{}, core.Time{}, err
	}
	if end.Cmp(start) <= 0 {
		return core.Time{}, core.Time{}, tsError("cue end not after start: " + s)
	}
	return start, end, nil
}

// Parse decodes an SRT or WebVTT document. Format is sniffed from the
// WEBVTT signature. Malformed cues are isolated as recoverable FaultBadCue
// entries keyed by byte offset; a truncated final cue yields
// FaultTruncatedTail and every valid cue before it is kept.
func Parse(data []byte) ([]Cue, core.Faults) {
	lines := splitLines(data)
	isVTT := len(lines) > 0 && strings.HasPrefix(lines[0].text, "WEBVTT")
	blocks := groupBlocks(lines)
	var cues []Cue
	var faults core.Faults
	for bi, b := range blocks {
		if isVTT {
			head := strings.TrimSpace(b.lines[0])
			if bi == 0 && strings.HasPrefix(head, "WEBVTT") {
				continue // header block (may carry metadata lines)
			}
			if strings.HasPrefix(head, "NOTE") || strings.HasPrefix(head, "STYLE") ||
				strings.HasPrefix(head, "REGION") {
				continue
			}
		}
		cue, ok, truncated := parseBlock(b, isVTT)
		switch {
		case truncated:
			faults = append(faults, core.NewFault(core.FaultTruncatedTail, b.off, true,
				"final cue incomplete; preceding cues kept"))
		case !ok:
			faults = append(faults, core.NewFault(core.FaultBadCue, b.off, true,
				"malformed cue isolated: "+firstLine(b)))
		default:
			cues = append(cues, cue)
		}
	}
	return cues, faults
}

func firstLine(b block) string {
	if len(b.lines) == 0 {
		return ""
	}
	s := b.lines[0]
	if len(s) > 48 {
		s = s[:48] + "..."
	}
	return s
}

// parseBlock converts one block to a Cue. ok=false means malformed;
// truncated=true means the block looks like a cue cut off mid-write.
func parseBlock(b block, isVTT bool) (cue Cue, ok, truncated bool) {
	lines := b.lines
	// Optional SRT numeric index or VTT cue identifier.
	if _, err := strconv.Atoi(strings.TrimSpace(lines[0])); err == nil && !isVTT {
		lines = lines[1:]
	} else if isVTT && !strings.Contains(lines[0], "-->") && len(lines) > 1 {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return Cue{}, false, false
	}
	start, end, err := parseTimeline(lines[0])
	if err != nil {
		// A lone partial line at the end of the file is a truncated tail.
		if len(lines) == 1 {
			return Cue{}, false, true
		}
		return Cue{}, false, false
	}
	text := strings.Join(lines[1:], "\n")
	if strings.TrimSpace(text) == "" {
		// Timing line present but payload never arrived: truncated tail.
		return Cue{}, false, true
	}
	return Cue{Start: start, End: end, Text: text}, true, false
}

// Crop returns the cues overlapping [start, end), clamped to that window
// and shifted so the window start becomes zero — the per-segment view on
// the unified timeline.
func Crop(cues []Cue, start, end core.Time) []Cue {
	var out []Cue
	for _, c := range cues {
		if c.End.Cmp(start) <= 0 || c.Start.Cmp(end) >= 0 {
			continue
		}
		s, e := c.Start, c.End
		if s.Cmp(start) < 0 {
			s = start
		}
		if e.Cmp(end) > 0 {
			e = end
		}
		out = append(out, Cue{Start: s.Sub(start), End: e.Sub(start), Text: c.Text})
	}
	return out
}
