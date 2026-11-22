// Package subtitle parses SRT and WebVTT subtitle files into a single
// timeline event model. Parsers are streaming and fault tolerant: invalid
// time lines, overlaps and truncated records are reported as Fault values
// while scanning continues at the next recoverable record. Every event
// keeps its raw start/end byte offsets in the source file.
package subtitle

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"mediascan/internal/fault"
)

// Event is one subtitle record on the unified timeline.
type Event struct {
	Index     int    // sequence index, 1-based in file order
	StartMS   int64  // start time in milliseconds
	EndMS     int64  // end time in milliseconds
	Text      string // joined payload lines, newline separated
	RawStart  int64  // byte offset of the record's first byte
	RawEnd    int64  // byte offset immediately after the record
	StartLine int    // 1-based line number where the record begins
}

// Result holds everything a parser found.
type Result struct {
	Source string
	Events []Event
	Faults []fault.Fault
}

// scanner wraps bufio.Reader with accurate byte-offset tracking. It reads
// one physical line at a time so a truncated final line is still visible.
type scanner struct {
	r      *bufio.Reader
	pos    int64
	line   int
	src    string
	faults []fault.Fault
}

func newScanner(r io.Reader, src string) *scanner {
	return &scanner{r: bufio.NewReaderSize(r, 64*1024), src: src, line: 1}
}

// nextLine returns the line content without its trailing newline and the
// byte offset where it began. status is lineOK for any newline-terminated
// line (including the last one), lineTrunc for an unterminated final line,
// and lineDone for a read with no data left. Callers treat a lineOK whose
// following call returns lineDone as a clean end of file.
func (s *scanner) nextLine() (text string, start int64, status lineStatus, err error) {
	start = s.pos
	var buf strings.Builder
	for {
		b, e := s.r.ReadByte()
		if e == io.EOF {
			if buf.Len() == 0 {
				return "", start, lineDone, nil
			}
			return buf.String(), start, lineTrunc, nil
		}
		if e != nil {
			return "", start, lineDone, e
		}
		s.pos++
		if b == '\n' {
			s.line++
			return strings.TrimRight(buf.String(), "\r"), start, lineOK, nil
		}
		buf.WriteByte(b)
	}
}

type lineStatus int

const (
	lineOK    lineStatus = iota // newline-terminated line
	lineTrunc                   // unterminated final line
	lineDone                    // no data left
)

// skipBlankLines consumes blank separator lines and returns the first
// non-empty line's data; EOF is reported explicitly.
func (s *scanner) skipBlankLines() (text string, start int64, lineNo int, eof bool, err error) {
	for {
		t, st, status, e := s.nextLine()
		if e != nil {
			return "", 0, 0, false, e
		}
		if status == lineDone {
			return "", st, s.line, true, nil
		}
		if strings.TrimSpace(t) != "" {
			return t, st, s.line - 1, false, nil
		}
	}
}

func (s *scanner) addFault(offset int64, recoverable bool, format string, args ...any) {
	s.faults = append(s.faults,
		fault.New(s.src, offset, fault.StageSubtitle, recoverable, format, args...))
}

// errSkipRecord tells the format-specific parser to discard the current
// record and resume at the next blank-line boundary.
var errSkipRecord = errors.New("record skipped")

// halfDayMS is the threshold for distinguishing a genuine overlap from a
// cross-midnight wrap: a cue jumping backwards by 12 hours or more is treated
// as the next day, not as overlapping the previous cue.
const halfDayMS int64 = 12 * 60 * 60 * 1000

// isMidnightWrap reports whether start, which precedes prevEnd, should be
// interpreted as a wrap past midnight rather than an overlap.
func isMidnightWrap(start, prevEnd int64) bool {
	return prevEnd >= 0 && start < prevEnd && prevEnd-start >= halfDayMS
}
