package subtitle

import (
	"strings"

	"mediad/diag"
)

// VTTParser is a streaming WebVTT parser. It accepts the WEBVTT header,
// NOTE/STYLE/REGION blocks (skipped), optional cue identifiers and millisecond
// dot-fraction timestamps. The same chunk-boundary guarantees as SRT apply.
type VTTParser struct {
	baseParser
	headerDone bool
	// identifier holds an optional cue id line while waiting for the timing
	// header; WebVTT allows any text there, unlike SRT's numeric index.
	identifier string
	idOffset   int64
}

// NewVTT creates a parser bound to a track ID and the shared limit set.
func NewVTT(track string, limits diag.Limits) *VTTParser {
	p := &VTTParser{baseParser: newBaseParser(track, limits), idOffset: -1}
	p.handle = p.onLine
	return p
}

// Write ingests one chunk.
func (p *VTTParser) Write(chunk []byte) error { return p.baseParser.write(chunk) }

// Close signals EOF and returns completed cues.
func (p *VTTParser) Close() ([]Cue, error) {
	if p.pending.headerSeen {
		if err := p.finalizeCue(p.offset, p.buildCue); err != nil {
			return nil, err
		}
	}
	if p.pending.startOffset >= 0 || p.idOffset >= 0 {
		off := p.pending.startOffset
		if off < 0 {
			off = p.idOffset
		}
		return nil, diag.At(diag.ESubtitleTruncated, p.track, off,
			"unterminated WebVTT cue at EOF")
	}
	return p.baseParser.close()
}

func (p *VTTParser) onLine(line string) error {
	if err := p.countLine(); err != nil {
		return err
	}
	at := p.lastLineStart
	trimmed := strings.TrimSpace(line)

	if !p.headerDone {
		// The first non-empty line must start the WEBVTT signature.
		if trimmed == "" {
			return nil
		}
		if !strings.HasPrefix(trimmed, "WEBVTT") {
			return diag.At(diag.ESubtitleSyntax, p.track, at,
				"missing WEBVTT header, got %q", trimmed)
		}
		p.headerDone = true
		return nil
	}

	// Blank line ends a cue and clears any pending identifier.
	if trimmed == "" {
		if p.pending.headerSeen {
			err := p.finalizeCue(at, p.buildCue)
			p.identifier, p.idOffset, p.inMeta = "", -1, false
			return err
		}
		p.identifier, p.idOffset, p.inMeta = "", -1, false
		return nil
	}

	// Block-level constructs are consumed (and discarded) up to a blank line.
	if strings.HasPrefix(trimmed, "NOTE") || strings.HasPrefix(trimmed, "STYLE") ||
		strings.HasPrefix(trimmed, "REGION") {
		p.inMeta = true
		return nil
	}
	if p.inMeta {
		return nil // meta blocks end at the blank line handled above
	}

	// Timing header? WebVTT cues may start directly with timing, or carry an
	// identifier line first. A line containing --> is always the header.
	if strings.Contains(trimmed, "-->") {
		start, end, err := parseVTTRange(trimmed)
		if err != nil {
			return attachOffset(err, p.track, at)
		}
		if end < start {
			return diag.At(diag.ETimecode, p.track, at, "cue end %d precedes start %d", end, start)
		}
		off := at
		if p.idOffset >= 0 {
			off = p.idOffset
		}
		p.beginCue(off)
		p.pending.headerSeen = true
		p.pending.timingStart, p.pending.timingEnd = start, end
		p.identifier, p.idOffset = "", -1
		return nil
	}

	// Not a header and no cue open: this is an identifier line.
	if !p.pending.headerSeen && p.idOffset < 0 {
		p.identifier, p.idOffset = trimmed, at
		return nil
	}

	// Otherwise it is payload text.
	p.pending.text = append(p.pending.text, line)
	return nil
}

func (p *VTTParser) buildCue(b *cueBuilder) (Cue, error) {
	return Cue{
		Index: 0,
		Start: b.timingStart,
		End:   b.timingEnd,
		Text:  strings.Join(b.text, "\n"),
	}, nil
}

// parseVTTRange parses "00:00:01.000 --> 00:00:04.000" plus optional cue
// settings after the end timestamp ("line:90% position:50%").
func parseVTTRange(s string) (int64, int64, error) {
	pos := strings.Index(s, "-->")
	if pos < 0 {
		return 0, 0, diag.New(diag.ESubtitleSyntax, "", "missing --> in timing line %q", s)
	}
	left := strings.TrimSpace(s[:pos])
	right := strings.TrimSpace(s[pos+3:])
	if i := strings.IndexAny(right, " \t"); i >= 0 {
		right = right[:i]
	}
	// WebVTT permits MM:SS.mmm without an hour component; normalize it.
	if strings.Count(left, ":") == 1 {
		left = "00:" + left
	}
	if strings.Count(right, ":") == 1 {
		right = "00:" + right
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
