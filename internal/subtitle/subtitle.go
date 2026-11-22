// Package subtitle parses SRT and WebVTT cues onto the shared media
// timebase, preserving original offsets and reporting bad records with
// their source lines.
package subtitle

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxRecords bounds accepted cues; input beyond the limit is rejected.
const MaxRecords = 1_000_000

// Record is one parsed cue on the shared timebase.
type Record struct {
	Index     int    // 1-based cue number within its source
	Start     int64  // frames on the shared timebase
	End       int64  // frames on the shared timebase
	StartRaw  string // original offset text, preserved
	EndRaw    string // original offset text, preserved
	Text      string // cue payload (never written to logs)
	Source    string // origin, e.g. "srt" or "vtt"
	Line      int    // 1-based source line of the timing line
	Ambiguous bool   // duplicate timestamp kept with its source
}

// Reject describes a skipped unrecoverable record.
type Reject struct {
	Line   int
	Reason string
}

// Report is the parse result: accepted records plus rejected lines.
type Report struct {
	Records  []Record
	Rejected []Reject
}

// accum applies the shared overlap/duplicate/limit policy.
type accum struct {
	report Report
	seen   map[[2]int64]int // (start,end) -> index of first record
}

func newAccum() *accum { return &accum{seen: map[[2]int64]int{}} }

func (a *accum) add(rec Record) {
	if len(a.report.Records) >= MaxRecords {
		a.reject(rec.Line, "record limit exceeded")
		return
	}
	key := [2]int64{rec.Start, rec.End}
	if prev, ok := a.seen[key]; ok {
		// Duplicate timestamp: keep both, mark ambiguous with source.
		a.report.Records[prev].Ambiguous = true
		rec.Ambiguous = true
	} else {
		a.seen[key] = len(a.report.Records)	}
	if n := len(a.report.Records); n > 0 {
		last := a.report.Records[n-1]
		if rec.Start == last.Start && rec.End == last.End {
			// exact duplicate, kept above
		} else if rec.Start >= last.Start && rec.Start < last.End {
			// Starts inside the previous cue's interval: overlap.
			// (A start before last.Start is a new day after midnight
			// wrap, not an overlap.)
			a.reject(rec.Line, fmt.Sprintf("overlaps record %d", last.Index))
			return
		}
	}
	a.report.Records = append(a.report.Records, rec)
}

func (a *accum) reject(line int, reason string) {
	a.report.Rejected = append(a.report.Rejected, Reject{Line: line, Reason: reason})
}

// parseTimestamp parses "HH:MM:SS.mmm", "MM:SS.mmm" (sep '.') or
// "HH:MM:SS,mmm" (sep ',') into milliseconds.
func parseTimestamp(s string, sep byte) (int64, error) {
	s = strings.TrimSpace(s)
	main, msPart, ok := strings.Cut(s, string(sep))
	if !ok {
		return 0, fmt.Errorf("missing %q milliseconds separator", sep)
	}
	if len(msPart) != 3 {
		return 0, fmt.Errorf("milliseconds must be 3 digits: %q", s)
	}
	ms, err := strconv.ParseInt(msPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad milliseconds: %q", s)
	}
	fields := strings.Split(main, ":")
	var h, m, sec int64
	switch len(fields) {
	case 3:
		h, err = strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad hours: %q", s)
		}
		fields = fields[1:]
		fallthrough
	case 2:
		m, err = strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad minutes: %q", s)
		}
		sec, err = strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad seconds: %q", s)
		}
	default:
		return 0, fmt.Errorf("want [HH:]MM:SS: %q", s)
	}
	if m > 59 || sec > 59 || h < 0 || m < 0 || sec < 0 {
		return 0, fmt.Errorf("field out of range: %q", s)
	}
	return ((h*60+m)*60+sec)*1000 + ms, nil
}

// splitTiming splits "start --> end [settings]" and returns both raw
// timestamp strings.
func splitTiming(line string) (startRaw, endRaw string, err error) {
	left, right, ok := strings.Cut(line, "-->")
	if !ok {
		return "", "", fmt.Errorf("missing -->")
	}
	startRaw = strings.TrimSpace(left)
	// WebVTT allows cue settings after the end time; drop them.
	endRaw = strings.TrimSpace(strings.Fields(right)[0])
	if startRaw == "" || endRaw == "" {
		return "", "", fmt.Errorf("empty timestamp")
	}
	return startRaw, endRaw, nil
}
