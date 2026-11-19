// Package subtitle parses SRT and WebVTT cues while preserving the raw
// byte offset of every record. Malformed records are reported with
// their offset and the parser continues with the next recoverable
// record; nothing is silently repaired.
package subtitle

import (
	"bytes"
	"strings"

	"pairflow/media/internal/clock"
)

// Cue is one subtitle record on the shared rational time axis.
type Cue struct {
	Index    int            `json:"index"`
	Track    string         `json:"track"`
	Format   string         `json:"format"`
	Start    clock.Rational `json:"start"`
	End      clock.Rational `json:"end"`
	Text     string         `json:"text"`
	Offset   int64          `json:"offset"`
	EndOff   int64          `json:"end_offset"`
	Sequence string         `json:"sequence,omitempty"`
	Settings string         `json:"settings,omitempty"`
}

// Document is a parsed subtitle file plus every issue encountered.
type Document struct {
	Format    string               `json:"format"`
	Track     string               `json:"track"`
	Cues      []Cue                `json:"cues"`
	Errors    []*clock.SourceError `json:"errors"`
	BOM       bool                 `json:"bom"`
	CRLF      bool                 `json:"crlf"`
	Truncated bool                 `json:"truncated"`
}

// Recovered returns how many cue records were successfully parsed.
func (d *Document) Recovered() int { return len(d.Cues) }

// Duration returns the span covered by all cues.
func (d *Document) Duration() clock.Rational {
	if len(d.Cues) == 0 {
		return clock.Zero()
	}
	lo, hi := d.Cues[0].Start, d.Cues[0].End
	for _, c := range d.Cues[1:] {
		if c.Start.LT(lo) {
			lo = c.Start
		}
		if hi.LT(c.End) {
			hi = c.End
		}
	}
	if hi.LT(lo) {
		return clock.Zero()
	}
	return hi.Sub(lo)
}

// line is one physical line with its byte offset in the original file.
type line struct {
	text   string
	offset int64
	// truncated indicates the final line had no terminating newline
	// and the content did not look complete.
	truncated bool
}

// splitLines removes a UTF-8 BOM, splits on both LF and CRLF, and keeps
// the byte offset of every line. Offsets are relative to the original
// input including the BOM.
func splitLines(data []byte) (lines []line, bom, crlf bool) {
	base := int64(0)
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		bom = true
		base = 3
	}
	body := data[base:]
	if bytes.Contains(body, []byte("\r\n")) {
		crlf = true
	}
	start := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			text := string(body[start:i])
			if strings.HasSuffix(text, "\r") {
				text = text[:len(text)-1]
			}
			lines = append(lines, line{text: text, offset: base + int64(start)})
			start = i + 1
		}
	}
	if start < len(body) {
		text := string(body[start:])
		lines = append(lines, line{text: text, offset: base + int64(start), truncated: true})
	}
	// A trailing newline produces no empty record.
	return lines, bom, crlf
}

// timestamp parses [hh:]mm:ss[,.]mmm, [hh:]mm:ss[,.]fff or a bare
// seconds value. It returns the time in exact rational seconds.
func timestamp(s string) (clock.Rational, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return clock.Zero(), false
	}
	// Optional hour segment: the last 3 colon-separated groups are
	// mm, ss and the fractional part.
	parts := strings.Split(s, ":")
	var hh, mm int64
	var secPart string
	switch len(parts) {
	case 3:
		h, ok1 := parseInt(parts[0])
		m, ok2 := parseInt(parts[1])
		if !ok1 || !ok2 || h < 0 || m < 0 {
			return clock.Zero(), false
		}
		hh, mm, secPart = h, m, parts[2]
	case 2:
		m, ok := parseInt(parts[0])
		if !ok || m < 0 {
			return clock.Zero(), false
		}
		mm, secPart = m, parts[1]
	case 1:
		secPart = parts[0]
	default:
		return clock.Zero(), false
	}
	if mm > 59 {
		return clock.Zero(), false
	}
	sepIdx := strings.IndexAny(secPart, ",.")
	var ss int64
	var frac, scale int64
	if sepIdx >= 0 {
		secStr := secPart[:sepIdx]
		fracStr := secPart[sepIdx+1:]
		v, ok := parseInt(secStr)
		if !ok || v < 0 || v > 59 {
			return clock.Zero(), false
		}
		ss = v
		if len(fracStr) == 0 || len(fracStr) > 9 {
			return clock.Zero(), false
		}
		f, ok := parseInt(fracStr)
		if !ok || f < 0 {
			return clock.Zero(), false
		}
		frac = f
		scale = 1
		for i := 0; i < len(fracStr); i++ {
			scale *= 10
		}
	} else {
		v, ok := parseInt(secPart)
		if !ok || v < 0 || v > 59 {
			return clock.Zero(), false
		}
		ss = v
		scale = 1
	}
	total := clock.NewRational(hh*3600+mm*60+ss, 1)
	if frac != 0 {
		total = total.Add(clock.NewRational(frac, scale))
	}
	return total, true
}

func parseInt(s string) (int64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	var out int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		out = out*10 + int64(c-'0')
	}
	return out, true
}
