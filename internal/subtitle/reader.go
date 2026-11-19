// Package subtitle streams SRT and WebVTT cues into records on the archive
// tick grid. Parsing is byte-offset aware and never buffers a whole file:
// cues are handed to a callback as soon as their block closes, so callers
// apply backpressure simply by blocking or returning an error.
package subtitle

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"unicode/utf8"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
)

// DefaultMaxBlockBytes caps the text carried by a single cue. A block over
// the cap is truncated and reported; together with the pipeline queue cap it
// bounds total memory regardless of input size.
const DefaultMaxBlockBytes = 1 << 20 // 1 MiB

// Record is one parsed cue anchored in archive ticks.
type Record struct {
	Track    string
	Source   string
	Sequence int   // 1-based emission order within the source
	Offset   int64 // byte offset of the block's first byte
	Start    clock.Tick
	End      clock.Tick
	Text     string
}

// Parser is a single-use streaming parser.
type Parser struct {
	src    string
	r      *lineReader
	coll   *diag.Collector
	maxBlk int
	seq    int
	vtt    bool
}

// NewParser configures a parser. coll may be nil when the caller does not
// want structured recoverable errors collected.
func NewParser(r io.Reader, source string, coll *diag.Collector) *Parser {
	return &Parser{
		src:    source,
		r:      &lineReader{r: bufio.NewReaderSize(r, 64*1024)},
		coll:   coll,
		maxBlk: DefaultMaxBlockBytes,
	}
}

func (p *Parser) fail(offset int64, op, msg string, cause error) *diag.Error {
	e := diag.New(diag.PhaseParse, p.src, op, msg, true)
	if cause != nil {
		e = diag.Wrap(diag.PhaseParse, p.src, op, true, cause)
	}
	e = e.At(p.src, offset)
	if p.coll != nil {
		p.coll.Add(e)
	}
	return e
}

// Parse drives emit for every well-formed cue. It returns the first
// unrecoverable condition (context cancellation or emitter failure);
// recoverable input problems are reported through the collector.
func (p *Parser) Parse(ctx context.Context, emit func(Record) error) error {
	if err := p.stripBOM(); err != nil && err != io.EOF {
		return err
	}
	p.detectVTT()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, off, err := p.nextNonBlank()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		timing := line
		if !bytes.Contains(timing, []byte("-->")) {
			// SRT sequence number or VTT cue identifier: the next line must
			// carry the timing arrow.
			more, moff, merr := p.r.readLine()
			if merr != nil && merr != io.EOF {
				return merr
			}
			if merr == io.EOF || !bytes.Contains(more, []byte("-->")) {
				p.fail(off, "cue.header", "cue without timing line, block skipped", nil)
				if merr == io.EOF {
					return nil
				}
				// A blank line already closed the bad block; draining further
				// would swallow the next, potentially valid cue.
				if len(bytes.TrimSpace(more)) > 0 {
					p.skipToBlank()
				}
				continue
			}
			timing, off = more, moff
		}
		start, end, ok := p.parseTiming(timing, off)
		if !ok {
			p.skipToBlank()
			continue
		}
		text, truncated := p.readText()
		p.seq++
		rec := Record{Source: p.src, Sequence: p.seq, Offset: off,
			Start: clock.MillisToTick(start), End: clock.MillisToTick(end), Text: text}
		if truncated {
			p.fail(off, "cue.size", "cue text exceeded the block limit and was truncated", diag.ErrOverflow)
		}
		if err := emit(rec); err != nil {
			return err
		}
	}
}

func (p *Parser) stripBOM() error {
	head, err := p.r.r.Peek(3)
	if err != nil && err != io.EOF {
		return err
	}
	if len(head) == 3 && head[0] == 0xEF && head[1] == 0xBB && head[2] == 0xBF {
		if _, err := p.r.r.Discard(3); err != nil {
			return err
		}
		p.r.offset += 3
	}
	return nil
}

func (p *Parser) detectVTT() {
	line, _, err := p.r.peekLine()
	if err == nil && strings.HasPrefix(string(line), "WEBVTT") {
		p.vtt = true
		p.r.pending = nil // consume the header; it is never a cue
	}
}

func (p *Parser) nextNonBlank() ([]byte, int64, error) {
	for {
		line, off, err := p.r.readLine()
		if err != nil {
			return nil, off, err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			return line, off, nil
		}
	}
}

func (p *Parser) skipToBlank() {
	for {
		line, _, err := p.r.readLine()
		if err != nil || len(bytes.TrimSpace(line)) == 0 {
			return
		}
	}
}

// readText gathers cue lines up to the next blank line. It enforces the
// block byte cap and sanitises truncated UTF-8 in place.
func (p *Parser) readText() (string, bool) {
	var b strings.Builder
	truncated := false
	for {
		line, off, err := p.r.readLine()
		if err != nil || len(bytes.TrimSpace(line)) == 0 {
			break
		}
		if !utf8.Valid(line) {
			p.fail(off, "cue.utf8", "invalid UTF-8 byte sequence replaced", diag.ErrTruncated)
			line = []byte(strings.ToValidUTF8(string(line), "�"))
		}
		if b.Len()+len(line)+1 > p.maxBlk {
			truncated = true
			room := p.maxBlk - b.Len()
			if room > 0 {
				b.Write(line[:room])
			}
			continue // keep draining the block without growing past the cap
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.Write(line)
	}
	return b.String(), truncated
}

// parseTiming returns start/end milliseconds. A missing or malformed end is
// recoverable: the cue is emitted zero-length so validation flags it.
func (p *Parser) parseTiming(line []byte, off int64) (int64, int64, bool) {
	parts := bytes.SplitN(line, []byte("-->"), 2)
	startMs, err := parseStamp(strings.TrimSpace(string(parts[0])))
	if err != nil {
		p.fail(off, "cue.start", "unparseable cue start time, block skipped", err)
		return 0, 0, false
	}
	right := strings.TrimSpace(string(parts[1]))
	if fields := strings.Fields(right); len(fields) > 0 {
		right = fields[0] // drop VTT cue settings
	}
	endMs, err := parseStamp(right)
	if err != nil {
		p.fail(off, "cue.end", "missing or invalid end time; zero-duration cue emitted", err)
		return startMs, startMs, true
	}
	if endMs < startMs {
		p.fail(off, "cue.end", "end precedes start; zero-duration cue emitted", diag.ErrOverflow)
		return startMs, startMs, true
	}
	return startMs, endMs, true
}

// parseStamp accepts HH:MM:SS.mmm, MM:SS.mmm and SRT's comma fraction.
func parseStamp(s string) (int64, error) {
	s = strings.ReplaceAll(s, ",", ".")
	cols := strings.Split(s, ":")
	if len(cols) < 2 || len(cols) > 3 {
		return 0, diag.ErrInvalid
	}
	var hh, mm int64
	var secStr string
	var err error
	if len(cols) == 3 {
		if hh, err = parseInt(cols[0]); err != nil {
			return 0, err
		}
		mm, err = parseInt(cols[1])
	} else {
		mm, err = parseInt(cols[0])
	}
	if err != nil {
		return 0, err
	}
	secStr = cols[len(cols)-1]
	dot := strings.IndexByte(secStr, '.')
	var ss, frac int64
	if dot < 0 {
		ss, err = parseInt(secStr)
		frac = 0
	} else {
		ss, err = parseInt(secStr[:dot])
		if err == nil {
			f := secStr[dot+1:]
			if len(f) == 0 || len(f) > 3 {
				return 0, diag.ErrInvalid
			}
			for len(f) < 3 {
				f += "0"
			}
			frac, err = parseInt(f)
		}
	}
	if err != nil || mm > 59 || ss > 59 {
		return 0, diag.ErrInvalid
	}
	return ((hh*60+mm)*60+ss)*1000 + frac, nil
}

func parseInt(s string) (int64, error) {
	if s == "" {
		return 0, diag.ErrInvalid
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, diag.ErrInvalid
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// lineReader splits a stream on LF, CRLF and bare CR while tracking the
// absolute byte offset of every line.
type lineReader struct {
	r          *bufio.Reader
	offset     int64
	pending    []byte // one line held by peekLine, returned by the next read
	pendingOff int64
}

func (l *lineReader) readLine() ([]byte, int64, error) {
	if l.pending != nil {
		line, off := l.pending, l.pendingOff
		l.pending = nil
		return line, off, nil
	}
	start := l.offset
	var buf []byte
	for {
		b, err := l.r.ReadByte()
		if err != nil {
			return buf, start, err // io.EOF terminates a final unterminated line
		}
		l.offset++
		if b == '\n' {
			return buf, start, nil
		}
		if b == '\r' {
			if nb, e := l.r.ReadByte(); e == nil {
				l.offset++
				if nb != '\n' {
					_ = l.r.UnreadByte()
					l.offset--
				}
			}
			return buf, start, nil
		}
		buf = append(buf, b)
	}
}

// peekLine reads the first line without consuming it from the parser's point
// of view; the next readLine call returns the same line.
func (l *lineReader) peekLine() ([]byte, int64, error) {
	line, off, err := l.readLine()
	if err != nil && err != io.EOF {
		return nil, off, err
	}
	l.pending = append(l.pending[:0], line...)
	l.pendingOff = off
	return line, off, err
}
