package subtitle

import (
	"io"
	"strconv"
	"strings"
)

// ParseVTT parses a WebVTT stream. The mandatory WEBVTT header and NOTE /
// STYLE / REGION blocks are consumed; cue identifiers, millisecond dot
// separators and optional cue settings are supported. Fault handling mirrors
// ParseSRT: bad cues are reported and skipped, later cues still parse.
func ParseVTT(r io.Reader, source string) (*Result, error) {
	sc := newScanner(r, source)
	res := &Result{Source: source}

	// Mandatory header: first non-empty line must start with "WEBVTT".
	header, start, _, eof, err := sc.skipBlankLines()
	if err != nil {
		return nil, err
	}
	if eof || !strings.HasPrefix(header, "WEBVTT") {
		sc.addFault(start, false, "missing WEBVTT header")
		res.Faults = sc.faults
		return res, nil
	}

	var prevEnd int64 = -1
	for {
		line, recStart, lineNo, eof, err := sc.skipBlankLines()
		if err != nil {
			return nil, err
		}
		if eof {
			break
		}
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "NOTE"), trimmed == "STYLE" || trimmed == "REGION":
			sc.drainToBlank()
			continue
		}

		ev := Event{RawStart: recStart, StartLine: lineNo}
		timing := trimmed
		if !strings.Contains(trimmed, "-->") {
			// Line is a cue identifier; the timing line follows.
			next, _, status, err := sc.nextLine()
			if err != nil {
				return nil, err
			}
			if status == lineDone || status == lineTrunc {
				sc.addFault(recStart, true, "truncated VTT cue: identifier %q without timing", truncate(trimmed))
				if status == lineTrunc {
					// No further records can exist.
					res.Faults = sc.faults
					return res, nil
				}
				break
			}
			timing = strings.TrimSpace(next)
		}
		ev, ok := sc.vttRecord(ev, timing, prevEnd)
		if !ok {
			sc.drainToBlank()
			continue
		}
		ev.Index = len(res.Events) + 1
		res.Events = append(res.Events, ev)
		prevEnd = ev.EndMS
	}
	res.Faults = sc.faults
	return res, nil
}

// vttRecord parses the timing line and payload of one cue.
func (sc *scanner) vttRecord(ev Event, timingLine string, prevEnd int64) (Event, bool) {
	fields := strings.Fields(timingLine)
	arrow := -1
	for i, f := range fields {
		if f == "-->" {
			arrow = i
			break
		}
	}
	if arrow < 1 || arrow >= len(fields)-1 {
		sc.addFault(ev.RawStart, true, "invalid VTT timing line: %q", truncate(timingLine))
		return Event{}, false
	}
	start, ok1 := parseVTTTime(fields[arrow-1])
	end, ok2 := parseVTTTime(fields[arrow+1])
	if !ok1 || !ok2 {
		sc.addFault(ev.RawStart, true, "unparseable VTT timestamps: %q", truncate(timingLine))
		return Event{}, false
	}
	if end <= start {
		sc.addFault(ev.RawStart, true, "VTT interval not positive: %d -> %d", start, end)
		return Event{}, false
	}
	if prevEnd >= 0 && start < prevEnd && !isMidnightWrap(start, prevEnd) {
		sc.addFault(ev.RawStart, true, "VTT overlap: cue starts %dms before previous end %dms", start, prevEnd)
	}
	ev.StartMS, ev.EndMS = start, end

	var text []string
	for {
		line, lineStart, status, err := sc.nextLine()
		if err != nil {
			return Event{}, false
		}
		switch status {
		case lineTrunc:
			text = append(text, line)
			ev.RawEnd = sc.pos
			sc.addFault(ev.RawStart, true, "truncated VTT cue at EOF")
			ev.Text = strings.Join(text, "\n")
			return ev, true
		case lineDone:
			ev.RawEnd = lineStart
			ev.Text = strings.Join(text, "\n")
			return ev, true
		case lineOK:
			if strings.TrimSpace(line) == "" {
				ev.RawEnd = lineStart
				ev.Text = strings.Join(text, "\n")
				return ev, true
			}
			text = append(text, line)
		}
	}
}

// parseVTTTime parses "MM:SS.mmm", "HH:MM:SS.mmm" and hour tokens longer
// than two digits. Fields are range-checked instead of being silently zeroed.
func parseVTTTime(token string) (int64, bool) {
	parts := strings.Split(token, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	nums := make([]int, 0, 4)
	for i, p := range parts {
		if i != len(parts)-1 {
			// Hour and minute tokens are plain integers.
			if strings.Contains(p, ".") {
				return 0, false
			}
			v, err := strconv.Atoi(p)
			if err != nil || v < 0 {
				return 0, false
			}
			nums = append(nums, v)
			continue
		}
		// Final token is SS.mmm with exactly three fractional digits.
		sec := strings.Split(p, ".")
		if len(sec) != 2 || len(sec[1]) != 3 {
			return 0, false
		}
		s, err := strconv.Atoi(sec[0])
		if err != nil || s < 0 {
			return 0, false
		}
		ms, err := strconv.Atoi(sec[1])
		if err != nil || ms > 999 {
			return 0, false
		}
		nums = append(nums, s, ms)
	}
	var h, m, s, ms int
	switch len(parts) {
	case 3: // HH:MM:SS.mmm -> nums = [h, m, s, ms]
		h, m, s, ms = nums[0], nums[1], nums[2], nums[3]
	case 2: // MM:SS.mmm -> nums = [m, s, ms]
		m, s, ms = nums[0], nums[1], nums[2]
	default:
		return 0, false
	}
	if m > 59 || s > 59 {
		return 0, false
	}
	return int64(h)*3_600_000 + int64(m)*60_000 + int64(s)*1_000 + int64(ms), true
}
