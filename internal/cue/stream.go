// Package cue streams SRT/WebVTT subtitle records. Bad timestamps,
// truncated lines and overlong records are reported with their source
// line and skipped; overlapping cues keep a stable order.
package cue

import (
	"bufio"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"deliver/internal/model"
)

// DefaultMaxRecord bounds one cue record in bytes.
const DefaultMaxRecord = 4096

// Cue is one subtitle record on the unified media timeline.
type Cue struct {
	Index   int // record ordinal, keeps overlap order stable
	StartMs model.MediaMs
	EndMs   model.MediaMs
	Text    string
	Line    int // source line of the timestamp row
}

// Size approximates the in-memory footprint for buffer accounting.
func (c Cue) Size() int { return 64 + len(c.Text) }

var timeRe = regexp.MustCompile(
	`(?:(\d{1,2}):)?(\d{1,2}):(\d{2})[,.](\d{3})\s+-->\s+(?:(\d{1,2}):)?(\d{1,2}):(\d{2})[,.](\d{3})`)

func toMs(h, m, s, ms string) int64 {
	hi, _ := strconv.Atoi(h)
	mi, _ := strconv.Atoi(m)
	si, _ := strconv.Atoi(s)
	msi, _ := strconv.Atoi(ms)
	return int64(hi*3600+mi*60+si)*1000 + int64(msi)
}

// Parse streams a cue file (SRT, or WebVTT when vtt is true). Records
// are delimited by blank lines. It returns valid cues in file order and
// one recoverable error per skipped record.
func Parse(r io.Reader, vtt bool, maxRecord int) ([]Cue, []error) {
	if maxRecord <= 0 {
		maxRecord = DefaultMaxRecord
	}
	br := bufio.NewReader(r)
	var cues []Cue
	var errs []error
	var rec []string
	recLine, recBytes, lineNo, idx := 0, 0, 0, 0
	headerDone := !vtt

	flush := func() {
		if len(rec) == 0 {
			return
		}
		defer func() { rec, recBytes = nil, 0 }()
		if !headerDone { // WEBVTT header block
			headerDone = true
			return
		}
		if recBytes > maxRecord {
			errs = append(errs, model.Recoverablef(model.StageCue, recLine, -1,
				"record exceeds %d bytes, skipped", maxRecord))
			return
		}
		tl := 0
		if !vtt && isIndex(rec[0]) && len(rec) > 1 {
			tl = 1 // SRT sequence number row
		}
		m := timeRe.FindStringSubmatch(rec[tl])
		if m == nil {
			errs = append(errs, model.Recoverablef(model.StageCue, recLine+tl, -1,
				"bad or truncated timestamp row %q", rec[tl]))
			return
		}
		start, end := toMs(m[1], m[2], m[3], m[4]), toMs(m[5], m[6], m[7], m[8])
		if end <= start {
			errs = append(errs, model.Recoverablef(model.StageCue, recLine+tl, start,
				"cue ends at or before its start"))
			return
		}
		cues = append(cues, Cue{Index: idx, StartMs: start, EndMs: end,
			Text: strings.Join(rec[tl+1:], "\n"), Line: recLine + tl})
		idx++
	}

	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" || err == nil {
			lineNo++
		}
		if line == "" {
			flush()
		} else {
			if len(rec) == 0 {
				recLine = lineNo
			}
			rec = append(rec, line)
			recBytes += len(line) + 1
		}
		if err != nil { // io.EOF or a truncated final line
			flush()
			if err != io.EOF {
				errs = append(errs, model.Recoverablef(model.StageCue, lineNo, -1,
					"read error: %v", err))
			}
			break
		}
	}
	return cues, errs
}

func isIndex(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// SortStable orders overlapping cues by (start, end) without disturbing
// the file order of equal keys.
func SortStable(cues []Cue) {
	sort.SliceStable(cues, func(i, j int) bool {
		if cues[i].StartMs != cues[j].StartMs {
			return cues[i].StartMs < cues[j].StartMs
		}
		return cues[i].EndMs < cues[j].EndMs
	})
}
