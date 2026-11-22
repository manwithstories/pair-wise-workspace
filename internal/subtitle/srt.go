package subtitle

import (
	"io"
	"strconv"
	"strings"
)

// ParseSRT parses an SRT stream. Invalid timing lines, inverted intervals,
// overlaps and truncated final records become recoverable Faults; parsing
// always continues with the next record.
func ParseSRT(r io.Reader, source string) (*Result, error) {
	sc := newScanner(r, source)
	res := &Result{Source: source}
	var prevEnd int64 = -1

	for {
		idLine, recStart, lineNo, eof, err := sc.skipBlankLines()
		if err != nil {
			return nil, err
		}
		if eof {
			break
		}
		ev := Event{RawStart: recStart, StartLine: lineNo}

		// Optional numeric sequence index.
		if _, perr := strconv.Atoi(strings.TrimSpace(idLine)); perr == nil {
			timing, _, status, terr := sc.nextLine()
			if terr != nil {
				return nil, terr
			}
			if status == lineDone {
				sc.addFault(recStart, true, "truncated SRT record: index %q without timing", idLine)
				break
			}
			if status == lineTrunc {
				// Timing exists but is the unterminated final line: it can
				// only be valid if a full timing range was written.
				if _, _, ok := parseSRTRange(timing); !ok {
					sc.addFault(recStart, true, "truncated SRT record at EOF: %q", truncate(timing))
					break
				}
			}
			ev, err = sc.srtRecord(ev, timing, prevEnd)
		} else {
			// Some encoders omit the index: treat this line as timing.
			ev, err = sc.srtRecord(ev, idLine, prevEnd)
		}
		if err != nil {
			if err == errSkipRecord {
				sc.drainToBlank()
				continue
			}
			return nil, err
		}
		ev.Index = len(res.Events) + 1
		res.Events = append(res.Events, ev)
		prevEnd = ev.EndMS
	}
	res.Faults = sc.faults
	return res, nil
}

// srtRecord parses one record starting at its timing line and reads payload
// lines until a blank separator or EOF.
func (sc *scanner) srtRecord(ev Event, timing string, prevEnd int64) (Event, error) {
	start, end, ok := parseSRTRange(timing)
	if !ok {
		sc.addFault(ev.RawStart, true, "invalid SRT timing line: %q", truncate(timing))
		return Event{}, errSkipRecord
	}
	if end <= start {
		sc.addFault(ev.RawStart, true, "SRT interval not positive: %d -> %d", start, end)
		return Event{}, errSkipRecord
	}
	if prevEnd >= 0 && start < prevEnd {
		sc.addFault(ev.RawStart, true, "SRT overlap: record starts %dms before previous end %dms", start, prevEnd)
	}
	ev.StartMS, ev.EndMS = start, end

	var text []string
	for {
		line, lineStart, status, err := sc.nextLine()
		if err != nil {
			return Event{}, err
		}
		switch status {
		case lineTrunc:
			// Unterminated final line: keep payload, mark truncation.
			text = append(text, line)
			ev.RawEnd = sc.pos
			sc.addFault(ev.RawStart, true, "truncated SRT record at EOF")
			ev.Text = strings.Join(text, "\n")
			return ev, nil
		case lineDone:
			ev.RawEnd = lineStart // clean end after the last newline
			ev.Text = strings.Join(text, "\n")
			return ev, nil
		case lineOK:
			if strings.TrimSpace(line) == "" {
				ev.RawEnd = lineStart // span excludes the separator line
				ev.Text = strings.Join(text, "\n")
				return ev, nil
			}
			text = append(text, line)
		}
	}
}

// drainToBlank consumes lines up to and including the next blank line so a
// failed record cannot poison the following one.
func (sc *scanner) drainToBlank() {
	for {
		line, _, status, _ := sc.nextLine()
		if status == lineDone || status == lineTrunc {
			return
		}
		if status == lineOK && strings.TrimSpace(line) == "" {
			return
		}
	}
}

// parseSRTRange parses "HH:MM:SS,mmm --> HH:MM:SS,mmm".
func parseSRTRange(line string) (start, end int64, ok bool) {
	parts := strings.Split(line, "-->")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, ok1 := parseSRTTime(strings.TrimSpace(parts[0]))
	end, ok2 := parseSRTTime(strings.TrimSpace(parts[1]))
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return start, end, true
}

// parseSRTTime parses HH:MM:SS,mmm and rejects out-of-range fields.
func parseSRTTime(token string) (int64, bool) {
	if len(token) != 12 || token[2] != ':' || token[5] != ':' || token[8] != ',' {
		return 0, false
	}
	h, e1 := strconv.Atoi(token[0:2])
	m, e2 := strconv.Atoi(token[3:5])
	s, e3 := strconv.Atoi(token[6:8])
	ms, e4 := strconv.Atoi(token[9:12])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return 0, false
	}
	if m > 59 || s > 59 || ms > 999 {
		return 0, false
	}
	return int64(h)*3_600_000 + int64(m)*60_000 + int64(s)*1_000 + int64(ms), true
}

func truncate(s string) string {
	const max = 48
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
