package subtitle

import (
	"strconv"
	"strings"

	"mediasync/internal/media"
	"mediasync/internal/timeline"
)

// Limits bound subtitle parsing work so a hostile input cannot exhaust memory.
const (
	MaxSubtitleBytes = 8 << 20
	MaxCues          = 100_000
	MaxCueText       = 64 << 10
)

type rawLine struct {
	text string
	off  int64
	num  int
}

func newIssue(stage media.Stage, input media.InputType, code media.Code, file string, off int64, rec int, format string, args ...any) *media.Issue {
	return media.New(stage, input, code, format, args...).WithFile(file).WithOffset(off).WithRecord(rec)
}

// splitLines normalises the input while retaining raw byte offsets.
func splitLines(raw []byte) []rawLine {
	lines := make([]rawLine, 0, 64)
	start := 0
	num := 1
	for i := 0; i <= len(raw); i++ {
		if i != len(raw) && raw[i] != '\n' {
			continue
		}
		text := string(raw[start:i])
		if strings.HasSuffix(text, "\r") {
			text = text[:len(text)-1]
		}
		if num == 1 {
			text = strings.TrimPrefix(text, "\uFEFF")
		}
		text = strings.ReplaceAll(text, "\x00", "")
		lines = append(lines, rawLine{text: text, off: int64(start), num: num})
		start = i + 1
		num++
	}
	return lines
}

func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// block is a candidate cue record delimited by blank lines.
type block struct {
	first, last int // inclusive indices into lines
}

func splitBlocks(lines []rawLine) []block {
	var blocks []block
	i := 0
	for i < len(lines) {
		if isBlank(lines[i].text) {
			i++
			continue
		}
		j := i
		for j < len(lines) && !isBlank(lines[j].text) {
			j++
		}
		blocks = append(blocks, block{first: i, last: j - 1})
		i = j
	}
	return blocks
}

type parser struct {
	res       *Result
	lines     []rawLine
	input     media.InputType
	file      string
	stage     media.Stage
	prevStart int64
	prevEnd   int64
	havePrev  bool
}

func (p *parser) issue(code media.Code, off int64, rec int, format string, args ...any) {
	p.res.Issues = append(p.res.Issues, newIssue(p.stage, p.input, code, p.file, off, rec, format, args...))
}

// cueFromBlock converts a block of lines into a cue. It returns false when the
// record is unrecoverable, having already recorded the issue.
func (p *parser) cueFromBlock(b block, allowIdentifier bool) (Cue, bool) {
	rec := len(p.res.Cues) + len(p.res.Issues) + 1
	idx := 0
	pos := b.first
	if allowIdentifier && pos+1 <= b.last && !strings.Contains(p.lines[pos].text, "-->") &&
		(p.input == media.InputWebVTT || isNumericIdentifier(p.lines[pos].text)) {
		if isNumericIdentifier(p.lines[pos].text) {
			idx, _ = strconv.Atoi(strings.TrimSpace(p.lines[pos].text))
		}
		pos++
	}
	timingIdx := -1
	for k := pos; k <= b.last; k++ {
		if strings.Contains(p.lines[k].text, "-->") {
			timingIdx = k
			break
		}
	}
	if timingIdx < 0 {
		p.issue(media.CodeMalformed, p.lines[b.first].off, rec,
			"block without a timing line (lines %d-%d)", p.lines[b.first].num, p.lines[b.last].num)
		return Cue{}, false
	}
	line := p.lines[timingIdx]
	if timingIdx > pos {
		// More than one non-timing line before the timing line: the record is
		// too damaged to trust.
		p.issue(media.CodeMalformed, p.lines[pos].off, rec,
			"unexpected content before timing line at line %d", line.num)
		return Cue{}, false
	}
	start, end, ok := p.parseTiming(line, rec)
	if !ok {
		return Cue{}, false
	}
	if end <= start {
		p.issue(media.CodeInvalidTimestamp, line.off, rec,
			"cue end %s is not after start %s", timeline.FormatClockTimestamp(end), timeline.FormatClockTimestamp(start))
		return Cue{}, false
	}
	var text []string
	size := 0
	for k := timingIdx + 1; k <= b.last; k++ {
		t := p.lines[k].text
		if size += len(t); size > MaxCueText {
			p.issue(media.CodeLimitExceeded, p.lines[k].off, rec, "cue text exceeds %d bytes", MaxCueText)
			return Cue{}, false
		}
		text = append(text, t)
	}
	cue := Cue{
		Index:     idx,
		StartNS:   start,
		EndNS:     end,
		Text:      strings.Join(text, "\n"),
		StartLine: line.num,
		Offset:    p.lines[b.first].off,
		EndOffset: p.lines[b.last].off + int64(len(p.lines[b.last].text)),
	}
	if p.havePrev {
		if start < p.prevStart {
			p.issue(media.CodeNonMonotonic, line.off, rec,
				"cue starts before the previous cue (%s < %s)",
				timeline.FormatClockTimestamp(start), timeline.FormatClockTimestamp(p.prevStart))
		}
		if start < p.prevEnd {
			cue.Overlap = true
			p.issue(media.CodeOverlap, line.off, rec,
				"cue overlaps previous cue by %dms", (p.prevEnd-start)/1_000_000)
		}
	}
	p.prevStart, p.prevEnd, p.havePrev = start, end, true
	return cue, true
}

func (p *parser) parseTiming(line rawLine, rec int) (start, end int64, ok bool) {
	parts := strings.SplitN(line.text, "-->", 2)
	startTok := strings.TrimSpace(parts[0])
	endField := strings.TrimSpace(parts[1])
	endTok := endField
	if i := strings.IndexAny(endField, " \t"); i >= 0 {
		endTok = endField[:i]
	}
	if p.input == media.InputWebVTT {
		for _, tok := range []string{startTok, endTok} {
			if strings.Contains(tok, ",") {
				p.issue(media.CodeInvalidTimestamp, line.off+int64(columnOf(line.text, tok)), rec,
					"WebVTT timestamps use '.' not ',' in %q", tok)
				return 0, 0, false
			}
		}
	}
	var err error
	start, err = timeline.ParseClockTimestamp(startTok)
	if err != nil {
		p.issue(media.CodeInvalidTimestamp, line.off+int64(columnOf(line.text, startTok)), rec,
			"bad start timestamp %q: %s", startTok, errText(err))
		return 0, 0, false
	}
	if endTok == "" {
		p.issue(media.CodeTruncated, line.off, rec, "timing line has no end timestamp")
		return 0, 0, false
	}
	end, err = timeline.ParseClockTimestamp(endTok)
	if err != nil {
		p.issue(media.CodeInvalidTimestamp, line.off+int64(columnOf(line.text, endTok)), rec,
			"bad end timestamp %q: %s", endTok, errText(err))
		return 0, 0, false
	}
	return start, end, true
}

func isNumericIdentifier(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if _, err := strconv.Atoi(s); err != nil {
		return false
	}
	return !strings.Contains(s, ":") && !strings.Contains(s, ".")
}

func columnOf(line, tok string) int {
	if i := strings.Index(line, tok); i >= 0 {
		return i
	}
	return 0
}

func errText(err error) string {
	if ie, ok := err.(*media.Issue); ok {
		return ie.Message
	}
	return err.Error()
}

func (p *parser) tooMany() bool {
	if len(p.res.Cues) >= MaxCues {
		p.issue(media.CodeLimitExceeded, 0, MaxCues, "more than %d cues; remaining records skipped", MaxCues)
		return true
	}
	return false
}

// Content returns the cue text with markup removed and whitespace collapsed.
func (c Cue) Content() string {
	var b strings.Builder
	inTag := false
	for _, r := range c.Text {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
