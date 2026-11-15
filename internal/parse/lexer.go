// Package parse implements self-contained lexers and parsers for
// SRT, WebVTT, ASS and TTML into the unified model.Cue timeline.
package parse

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"subtool/internal/model"
)

// Result is the output of any format parser.
type Result struct {
	Cues   []model.Cue
	Issues []model.Issue // quarantined entries with source line numbers
	Header map[string]string
}

// maxTimelineMs is the 24h ceiling; anything beyond is quarantined.
const maxTimelineMs = 24 * 3600 * 1000

var timecodeRE = regexp.MustCompile(
	`^(\d{1,3}):([0-5]?\d):([0-5]?\d)([.,])(\d{1,3})$`)

// ParseTimecode parses "HH:MM:SS,mmm" / "HH:MM:SS.mmm" / "H:MM:SS.cc"
// into rational milliseconds. Fractions are right-padded to 3 digits,
// so ASS centiseconds and VTT milliseconds share one path.
func ParseTimecode(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	m := timecodeRE.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("malformed timecode %q", s)
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	sec, _ := strconv.Atoi(m[3])
	frac := m[5]
	for len(frac) < 3 {
		frac += "0"
	}
	fx, _ := strconv.Atoi(frac)
	ms := int64(((h*60+mi)*60+sec))*1000 + int64(fx)
	return big.NewRat(ms, 1), nil
}

// SplitTiming splits "a --> b" (optionally followed by settings) into
// the two timecode strings and the trailing settings text.
func SplitTiming(line string) (start, end, settings string, err error) {
	idx := strings.Index(line, "-->")
	if idx < 0 {
		return "", "", "", fmt.Errorf("missing --> separator")
	}
	start = strings.TrimSpace(line[:idx])
	rest := strings.TrimSpace(line[idx+3:])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", "", fmt.Errorf("missing end timecode")
	}
	end = fields[0]
	settings = strings.TrimSpace(strings.TrimPrefix(rest, end))
	return start, end, settings, nil
}

// ValidateCue applies quarantine rules to a parsed cue. It returns a
// non-nil issue when the cue must be isolated instead of emitted.
func ValidateCue(c *model.Cue) *model.Issue {
	mk := func(code, msg string) *model.Issue {
		return &model.Issue{Line: c.SourceLine, Code: code, Msg: msg}
	}
	if c.Start == nil || c.End == nil {
		return mk(model.CodeMissing, "missing start or end timecode")
	}
	if c.Start.Sign() < 0 || c.End.Sign() < 0 {
		return mk(model.CodeNegative, "negative timecode")
	}
	limit := big.NewRat(maxTimelineMs, 1)
	if c.Start.Cmp(limit) > 0 || c.End.Cmp(limit) > 0 {
		return mk(model.CodeTooLong, "timecode beyond 24h")
	}
	switch c.End.Cmp(c.Start) {
	case -1:
		return mk(model.CodeEndBefore, "end earlier than start")
	case 0:
		return mk(model.CodeZeroLength, "zero-length cue")
	}
	return nil
}

// tagRE captures SRT/VTT-style inline tags and ASS override blocks.
var tagRE = regexp.MustCompile(`(?i)</?[a-z][^>]*>|\{[^}]*\}`)

// ExtractTags pulls inline tags out of text for lossless verification.
func ExtractTags(text string) []string {
	return tagRE.FindAllString(text, -1)
}

// NormalizeLines splits decoded text into lines, tolerating CRLF/LF/CR.
func NormalizeLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}

// Block is a blank-line-separated group of lines. Line is the 1-based
// source line number of the block's first line.
type Block struct {
	Line  int
	Lines []string
}

// Blocks groups lines into blank-line-separated blocks, tolerating
// missing blank lines is left to each format parser.
func Blocks(lines []string) []Block {
	var out []Block
	var cur []string
	start := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, Block{Line: start, Lines: cur})
			cur = nil
		}
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		if len(cur) == 0 {
			start = i + 1
		}
		cur = append(cur, ln)
	}
	flush()
	return out
}
