// Package subtitle parses SRT and WebVTT cue streams incrementally.
//
// The parsers are streaming: data may arrive in chunks of any size (a single
// byte at a time works identically to one large buffer), and all state lives
// in the parser between Write calls. Every error carries the absolute byte
// offset inside the track's input, the track ID and a stable error code, so a
// truncated or malformed cue can be located precisely. A failure on one track
// never prevents other tracks from being parsed: parsers are independent
// state machines with no shared mutable state.
package subtitle

import (
	"strings"

	"mediad/diag"
)

// Cue is one subtitle event on the unified millisecond time base.
type Cue struct {
	// Index is the declared sequence number (SRT) or zero for WebVTT.
	Index int
	Start int64 // inclusive start, milliseconds
	End   int64 // exclusive end, milliseconds
	Text  string
	// StartOffset/EndOffset are absolute byte offsets of the cue block.
	StartOffset int64
	EndOffset   int64
}

// cueBuilder accumulates one cue while its lines stream in.
type cueBuilder struct {
	index       int
	headerSeen  bool
	text        []string
	startOffset int64
	timingStart int64
	timingEnd   int64
}

// baseParser holds the machinery shared by SRT and WebVTT.
type baseParser struct {
	track     string
	buf       []byte // unconsumed bytes, never grows past one pending line set
	offset    int64  // absolute offset of the next byte to arrive
	lines     int    // total input lines accepted (limit enforcement)
	inMeta    bool   // inside a VTT NOTE/STYLE/REGION block
	pending   cueBuilder
	cues      []Cue
	closed        bool
	limits        diag.Limits
	handle        func(line string) error // format-specific line state machine
	lastLineStart int64                   // absolute offset of the current line
}

func newBaseParser(track string, limits diag.Limits) baseParser {
	return baseParser{track: track, limits: limits, pending: cueBuilder{startOffset: -1}}
}

// Write appends a chunk and drains every complete line (terminated by '\n').
// The terminator itself advances the offset so error offsets point at the
// first byte of the offending block.
func (p *baseParser) write(chunk []byte) error {
	if p.closed {
		return diag.New(diag.ESubtitleSyntax, p.track, "write after close")
	}
	p.buf = append(p.buf, chunk...)
	p.offset += int64(len(chunk))
	for {
		nl := indexByte(p.buf, '\n')
		if nl < 0 {
			return nil
		}
		// Absolute offset of this line's first byte: total seen minus the
		// bytes still buffered (including the newline about to be consumed).
		lineStart := p.offset - int64(len(p.buf))
		raw := p.buf[:nl]
		p.buf = p.buf[nl+1:]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1] // normalize CRLF
		}
		p.lastLineStart = lineStart
		if err := p.handle(string(raw)); err != nil {
			return err
		}
	}
}

// handleLine is supplied per format; shared blank-line finalization lives here.
func (p *baseParser) beginCue(at int64) {
	p.pending = cueBuilder{startOffset: at}
}

// finalizeCue emits the accumulated builder as a Cue.
func (p *baseParser) finalizeCue(endOffset int64, build func(b *cueBuilder) (Cue, error)) error {
	if !p.pending.headerSeen {
		return diag.At(diag.ESubtitleTruncated, p.track, p.pending.startOffset,
			"cue block has no timing header")
	}
	cue, err := build(&p.pending)
	if err != nil {
		return err
	}
	cue.StartOffset = p.pending.startOffset
	cue.EndOffset = endOffset
	p.cues = append(p.cues, cue)
	p.pending = cueBuilder{startOffset: -1}
	return nil
}

// close finalizes at EOF: a trailing cue without a blank line is valid in
// both formats, but a half-written header (no terminator) is truncation.
func (p *baseParser) close() ([]Cue, error) {
	p.closed = true
	if len(p.buf) > 0 {
		off := p.offset - int64(len(p.buf))
		return nil, diag.At(diag.ESubtitleTruncated, p.track, off,
			"unterminated trailing line of %d bytes", len(p.buf))
	}
	return p.cues, nil
}

// countLine enforces the configured line budget.
func (p *baseParser) countLine() error {
	p.lines++
	return p.limits.CheckCueLines(p.track, p.lines)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// parseMillis reads "HH:MM:SS,mmm" (SRT) or "HH:MM:SS.mmm" (VTT) and returns
// milliseconds. Components beyond hours are range checked.
func parseMillis(s string) (int64, error) {
	s = strings.TrimSpace(s)
	comma := strings.IndexByte(s, ',')
	dot := strings.IndexByte(s, '.')
	if comma >= 0 {
		s = s[:comma] + "." + s[comma+1:]
	} else if dot < 0 {
		return 0, diag.New(diag.ESubtitleSyntax, "", "timestamp %q missing fraction", s)
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, diag.New(diag.ESubtitleSyntax, "", "timestamp %q must be HH:MM:SS.mmm", s)
	}
	h, e1 := atoiNonNeg(parts[0])
	m, e2 := atoiNonNeg(parts[1])
	secParts := strings.SplitN(parts[2], ".", 2)
	sec, e3 := atoiNonNeg(secParts[0])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, diag.New(diag.ESubtitleSyntax, "", "timestamp %q has non-numeric component", s)
	}
	if m > 59 || sec > 59 {
		return 0, diag.New(diag.ETimecode, "", "timestamp %q out of range", s)
	}
	frac := secParts[1]
	if len(frac) != 3 {
		return 0, diag.New(diag.ESubtitleSyntax, "", "timestamp %q must have millisecond precision", s)
	}
	ms, err := atoiNonNeg(frac)
	if err != nil {
		return 0, diag.New(diag.ESubtitleSyntax, "", "timestamp %q bad fraction", s)
	}
	return ((h*60+m)*60+sec)*1000 + ms, nil
}

func atoiNonNeg(s string) (int64, error) {
	if s == "" {
		return 0, errBad
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBad
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

var errBad = diag.New(diag.ESubtitleSyntax, "", "invalid number")
