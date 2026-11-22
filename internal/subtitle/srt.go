// Package subtitle parses SRT and WebVTT cue files onto the unified
// nanosecond time base and reports a source byte offset for every
// error. Floating-point arithmetic is never used.
package subtitle

import (
	"bytes"
	"fmt"
	"strings"

	"mediasync/internal/clock"
)

// Cue is one subtitle cue.
type Cue struct {
	Index  int
	Start  int64 // nanoseconds on the unified time base
	End    int64
	Text   string
	Offset int64 // byte offset of the cue block in the source
}

// ParseError locates a malformed cue by byte offset.
type ParseError struct {
	Offset int64
	Kind   string // "timestamp", "duration" or "order"
	Msg    string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("subtitle: %s error at byte %d: %s", e.Kind, e.Offset, e.Msg)
}

// ParseSRT parses an SRT document. On error it returns the cues parsed
// so far together with a *ParseError, so callers can salvage a prefix.
func ParseSRT(src []byte) ([]Cue, error) {
	ls, offs := splitLines(src)
	var cues []Cue
	i := 0
	for {
		for i < len(ls) && strings.TrimSpace(ls[i]) == "" {
			i++
		}
		if i >= len(ls) {
			return cues, nil
		}
		cueOff := offs[i]
		idx, ok := parseIndex(ls[i])
		if !ok {
			return cues, &ParseError{Offset: offs[i], Kind: "timestamp", Msg: "bad cue index: " + ls[i]}
		}
		i++
		if i >= len(ls) {
			return cues, &ParseError{Offset: cueOff, Kind: "timestamp", Msg: "missing timing line"}
		}
		start, end, err := parseTiming(ls[i], offs[i], ',', false)
		if err != nil {
			return cues, err
		}
		i++
		var text []string
		for i < len(ls) && strings.TrimSpace(ls[i]) != "" {
			text = append(text, ls[i])
			i++
		}
		cue := Cue{Index: idx, Start: start, End: end, Text: strings.Join(text, "\n"), Offset: cueOff}
		if err := checkOrder(cue, cues); err != nil {
			return cues, err
		}
		cues = append(cues, cue)
	}
}

// parseTiming parses "TS --> TS" on one line. sep is the millisecond
// separator (',' for SRT, '.' for WebVTT); short allows the MM:SS.mmm
// WebVTT shorthand. A non-positive duration is reported at the line
// offset, a malformed end token at the end token's offset.
func parseTiming(l string, off int64, sep byte, short bool) (int64, int64, error) {
	const arrow = " --> "
	ai := strings.Index(l, arrow)
	if ai < 0 {
		return 0, 0, &ParseError{Offset: off, Kind: "timestamp", Msg: "missing --> in " + l}
	}
	stok := strings.TrimSpace(l[:ai])
	etok := firstField(strings.TrimSpace(l[ai+len(arrow):]))
	if short {
		if len(stok) == 9 {
			stok = "00:" + stok
		}
		if len(etok) == 9 {
			etok = "00:" + etok
		}
	}
	start, err := parseHMS(stok, sep)
	if err != nil {
		return 0, 0, &ParseError{Offset: off, Kind: "timestamp", Msg: err.Error()}
	}
	end, err := parseHMS(etok, sep)
	if err != nil {
		return 0, 0, &ParseError{Offset: off + int64(ai+len(arrow)), Kind: "timestamp", Msg: err.Error()}
	}
	if end <= start {
		return 0, 0, &ParseError{Offset: off, Kind: "duration",
			Msg: fmt.Sprintf("non-positive duration: start %d end %d", start, end)}
	}
	return start, end, nil
}

// parseHMS parses "HH:MM:SS<sep>mmm" into nanoseconds.
func parseHMS(s string, sep byte) (int64, error) {
	if len(s) != 12 || s[2] != ':' || s[5] != ':' || s[8] != sep {
		return 0, fmt.Errorf("malformed timestamp %q", s)
	}
	var v [4]int64
	for i, span := range [][2]int{{0, 2}, {3, 5}, {6, 8}, {9, 12}} {
		for k := span[0]; k < span[1]; k++ {
			if s[k] < '0' || s[k] > '9' {
				return 0, fmt.Errorf("malformed timestamp %q", s)
			}
			v[i] = v[i]*10 + int64(s[k]-'0')
		}
	}
	if v[1] > 59 || v[2] > 59 {
		return 0, fmt.Errorf("timestamp %q field out of range", s)
	}
	return ((v[0]*60+v[1])*60+v[2])*clock.NanosPerSec + v[3]*1_000_000, nil
}

func checkOrder(c Cue, prev []Cue) error {
	if n := len(prev); n > 0 && c.Start < prev[n-1].Start {
		return &ParseError{Offset: c.Offset, Kind: "order",
			Msg: fmt.Sprintf("cue starts at %d before previous cue at %d", c.Start, prev[n-1].Start)}
	}
	return nil
}

func parseIndex(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func firstField(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// splitLines splits src into lines with their byte offsets.
func splitLines(src []byte) ([]string, []int64) {
	var ls []string
	var offs []int64
	for off := 0; off < len(src); {
		j := bytes.IndexByte(src[off:], '\n')
		var l string
		var next int
		if j < 0 {
			l = string(src[off:])
			next = len(src)
		} else {
			l = string(src[off : off+j])
			next = off + j + 1
		}
		ls = append(ls, strings.TrimRight(l, "\r"))
		offs = append(offs, int64(off))
		off = next
	}
	return ls, offs
}
