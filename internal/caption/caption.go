// Package caption parses SRT and WebVTT subtitle feeds into cues positioned
// on the shared 90 kHz integer time base. Parsers are streaming: they operate
// on an io.Reader with a small line buffer, so two-hour inputs stay within
// the resident-memory budget. Malformed records are reported with line and
// byte offsets and skipped individually; only a structurally unrecoverable
// stream aborts the parse.
package caption

import (
	"bufio"
	"io"

	"avcore/internal/adapter"
)

// Format identifies the caption dialect.
type Format string

const (
	FormatSRT   Format = "srt"
	FormatWebVTT Format = "vtt"
)

// Cue is one subtitle interval on the 90 kHz time base.
type Cue struct {
	Index       int    `json:"index"`
	StartTicks  int64  `json:"start_ticks"`
	EndTicks    int64  `json:"end_ticks"`
	Text        string `json:"text"`
	SourceLine  int    `json:"source_line"`
}

// BadRecord is a recoverable parse failure; the offending record was skipped.
type BadRecord struct {
	Code     adapter.ErrorCode `json:"code"`
	Message  string            `json:"message"`
	Line     int               `json:"line"`
	Offset   int64             `json:"offset"`
}

// Result is the full outcome of parsing one caption stream.
type Result struct {
	Format     Format      `json:"format"`
	Cues       []Cue       `json:"cues"`
	BadRecords []BadRecord `json:"bad_records"`
	// Fatal is set only when the whole stream cannot be parsed at all.
	Fatal      *adapter.Error `json:"fatal,omitempty"`
}

// MaxCaptionBytes bounds a single caption feed. Larger inputs are rejected
// outright instead of silently exhausting memory (two-hour text feeds are
// well below this in practice).
const MaxCaptionBytes int64 = 32 << 20 // 32 MiB

// lineReader wraps bufio.Reader while tracking the absolute byte offset and
// the 1-based line number of every returned line.
type lineReader struct {
	r      *bufio.Reader
	offset int64
	line   int
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// nextLine returns the line content without its trailing newline, the line
// number and the byte offset at which the line started.
func (l *lineReader) nextLine() (text string, lineNo int, lineStart int64, err error) {
	lineStart = l.offset
	var raw []byte
	raw, err = l.r.ReadBytes('\n')
	l.line++
	l.offset += int64(len(raw))
	// Trim either CRLF or LF; a final line without newline is still valid.
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	if n := len(raw); n > 0 && raw[n-1] == '\r' {
		raw = raw[:n-1]
	}
	return string(raw), l.line, lineStart, err
}

// offsetOf reports the current absolute byte position.
func (l *lineReader) offsetOf() int64 { return l.offset }

func bad(code adapter.ErrorCode, msg string, line int, off int64) BadRecord {
	return BadRecord{Code: code, Message: msg, Line: line, Offset: off}
}
