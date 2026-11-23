package subtitle

import (
	"strconv"
	"strings"

	"mediad/diag"
)

// SRTParser is a streaming SubRip parser. Feed arbitrary chunk boundaries via
// Write; call Close at EOF to retrieve completed cues or a located error.
type SRTParser struct {
	baseParser
}

// NewSRT creates a parser bound to a track ID and the shared limit set.
func NewSRT(track string, limits diag.Limits) *SRTParser {
	p := &SRTParser{baseParser: newBaseParser(track, limits)}
	p.handle = p.onLine
	return p
}

// Write ingests one chunk; a returned error is terminal for this parser.
func (p *SRTParser) Write(chunk []byte) error {
	return p.baseParser.write(chunk)
}

// Close signals EOF and returns every fully parsed cue.
func (p *SRTParser) Close() ([]Cue, error) {
	if p.pending.headerSeen || (p.pending.startOffset >= 0 && len(p.pending.text) > 0) {
		if err := p.finalizeCue(p.offset, p.buildCue); err != nil {
			return nil, err
		}
	}
	// A number line never followed by a timing header is a truncated cue.
	if p.pending.startOffset >= 0 {
		return nil, diag.At(diag.ESubtitleTruncated, p.track, p.pending.startOffset,
			"cue index without timing header at EOF")
	}
	return p.baseParser.close()
}

func (p *SRTParser) onLine(line string) error {
	if err := p.countLine(); err != nil {
		return err
	}
	at := p.lastLineStart // first byte of this physical line
	trimmed := strings.TrimSpace(line)

	// Blank line: terminates an in-flight cue; leading blanks are tolerated.
	if trimmed == "" {
		if p.pending.headerSeen {
			return p.finalizeCue(at, p.buildCue)
		}
		if p.pending.startOffset >= 0 {
			// A number line followed directly by a blank line = truncated cue.
			return diag.At(diag.ESubtitleTruncated, p.track, p.pending.startOffset,
				"cue index without timing header")
		}
		return nil
	}

	if !p.pending.headerSeen {
		if p.pending.startOffset < 0 {
			// First line of a new block must be the numeric sequence index.
			idx, err := strconv.Atoi(trimmed)
			if err != nil || idx < 0 {
				return diag.At(diag.ESubtitleSyntax, p.track, at,
					"expected numeric cue index, got %q", trimmed)
			}
			p.beginCue(at)
			p.pending.index = idx
			return nil
		}
		// Second line must be the timing header.
		start, end, err := parseSRTRange(trimmed)
		if err != nil {
			return attachOffset(err, p.track, at)
		}
		if end < start {
			return diag.At(diag.ETimecode, p.track, at, "cue end %d precedes start %d", end, start)
		}
		p.pending.headerSeen = true
		p.pending.timingStart, p.pending.timingEnd = start, end
		return nil
	}

	// Header already seen: every further line is cue text.
	p.pending.text = append(p.pending.text, line)
	return nil
}

func (p *SRTParser) buildCue(b *cueBuilder) (Cue, error) {
	return Cue{
		Index: b.index,
		Start: b.timingStart,
		End:   b.timingEnd,
		Text:  strings.Join(b.text, "\n"),
	}, nil
}

// parseSRTRange parses "00:00:01,000 --> 00:00:04,000" (trailing settings
// after the end timestamp are allowed and ignored).
func parseSRTRange(s string) (int64, int64, error) {
	pos := strings.Index(s, "-->")
	if pos < 0 {
		return 0, 0, diag.New(diag.ESubtitleSyntax, "", "missing --> in timing line %q", s)
	}
	left := strings.TrimSpace(s[:pos])
	right := strings.TrimSpace(s[pos+3:])
	if i := strings.IndexAny(right, " \t"); i >= 0 {
		right = right[:i]
	}
	start, err := parseMillis(left)
	if err != nil {
		return 0, 0, err
	}
	end, err := parseMillis(right)
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

func attachOffset(err error, track string, offset int64) error {
	de, ok := err.(*diag.Error)
	if !ok {
		return err
	}
	cp := *de
	cp.Track = track
	cp.Offset = offset
	return &cp
}
