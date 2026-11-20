package frame

import (
	"bufio"
	"encoding/json"
	"io"
)

// lineScanner is a tiny allocation-light JSONL scanner used by the metadata
// loader. It tracks the byte offset of the current line so malformed entries
// can be reported with their exact position.
type lineScanner struct {
	br       *bufio.Reader
	line     []byte
	lineAt   int64
	off      int64
	lineNo   int
	errState error
}

func newLineScanner(r io.Reader) *lineScanner {
	return &lineScanner{br: bufio.NewReaderSize(r, 64<<10)}
}

// next advances to the next non-empty line.
func (s *lineScanner) next() bool {
	if s.errState != nil {
		return false
	}
	for {
		start := s.off
		line, err := s.br.ReadBytes('\n')
		s.off += int64(len(line))
		s.lineNo++
		if trimmed := trimSpace(line); len(trimmed) > 0 {
			s.line = trimmed
			s.lineAt = start
			return true
		}
		if err != nil {
			if err != io.EOF {
				s.errState = err
			}
			return false
		}
	}
}

func (s *lineScanner) unmarshal(v any) error { return json.Unmarshal(s.line, v) }

func (s *lineScanner) offset() int64 { return s.lineAt }

func (s *lineScanner) err() error {
	if s.errState == io.EOF {
		return nil
	}
	return s.errState
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}
