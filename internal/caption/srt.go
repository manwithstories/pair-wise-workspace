package caption

import (
	"bytes"
	"io"
	"strconv"
	"strings"

	"avcore/internal/adapter"
	"avcore/internal/clock"
)

// ParseSRT streams an SRT document. It tolerates bad individual records
// (bad timestamps, truncated payloads, overlaps) and records each one in
// BadRecords rather than failing the whole feed.
func ParseSRT(r io.Reader, maxBytes int64) Result {
	res := Result{Format: FormatSRT}
	lr := newLineReader(r)
	var cueSeq int

	for {
		// Skip blank separator lines between records.
		line, lineNo, lineOff, err := lr.nextLine()
		if err == io.EOF && line == "" {
			return res
		}
		if err != nil && err != io.EOF {
			res.Fatal = adapter.NewError(adapter.CodeInternal, "caption read failure: "+err.Error(),
				adapter.Source{Line: lineNo, Offset: lineOff})
			return res
		}
		if lr.offsetOf() > maxBytes {
			res.Fatal = adapter.NewError(adapter.CodeOversize,
				"caption stream exceeds size limit", adapter.Source{})
			return res
		}
		if strings.TrimSpace(line) == "" {
			if err == io.EOF {
				return res
			}
			continue
		}

		// First non-blank line of a record is its sequence number.
		idxLine, idxOff := lineNo, lineOff
		if _, perr := strconv.Atoi(strings.TrimSpace(line)); perr != nil {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeTruncated,
				"expected numeric cue index, got "+quote(line), idxLine, idxOff))
			if err == io.EOF {
				return res
			}
			continue
		}

		// Timing line.
		tLine, tNo, tOff, terr := lr.nextLine()
		if terr == io.EOF {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeTruncated,
				"cue missing timing line", tNo, tOff))
			return res
		}
		start, end, terr2 := parseSRTRange(tLine)
		if terr2 != nil {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeBadTimestamp,
				terr2.Error(), tNo, tOff))
			skipToNextRecord(lr)
			continue
		}

		// Text lines until the next blank line.
		var text bytes.Buffer
		textStart := lr.offsetOf()
		hitEOF := false
		for {
			txt, _, _, rerr := lr.nextLine()
			if strings.TrimSpace(txt) == "" {
				if rerr == io.EOF {
					hitEOF = true
				}
				break
			}
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(txt)
			if rerr == io.EOF {
				hitEOF = true
				break
			}
		}
		if hitEOF && text.Len() == 0 {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeTruncated,
				"cue has no text payload", tNo, textStart))
			return res
		}
		cueSeq++
		cue := Cue{
			Index:      cueSeq,
			StartTicks: start,
			EndTicks:   end,
			Text:       text.String(),
			SourceLine: idxLine,
		}
		if len(res.Cues) > 0 && start < res.Cues[len(res.Cues)-1].EndTicks {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeOverlap,
				"cue overlaps previous cue", idxLine, idxOff))
		}
		res.Cues = append(res.Cues, cue)
	}
}

// parseSRTRange parses "HH:MM:SS,mmm --> HH:MM:SS,mmm".
func parseSRTRange(line string) (int64, int64, error) {
	parts := strings.SplitN(line, "-->", 2)
	if len(parts) != 2 {
		return 0, 0, &parseError{"missing --> separator"}
	}
	start, err := parseSRTTime(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	end, err := parseSRTTime(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	if end <= start {
		return 0, 0, &parseError{"cue end must be after start"}
	}
	return start, end, nil
}

func parseSRTTime(s string) (int64, error) {
	fields := strings.Split(s, ":")
	if len(fields) != 3 {
		return 0, &parseError{"want HH:MM:SS,mmm: " + quote(s)}
	}
	h, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, &parseError{"bad hour: " + quote(s)}
	}
	m, err := strconv.Atoi(fields[1])
	if err != nil || m < 0 || m > 59 {
		return 0, &parseError{"bad minute: " + quote(s)}
	}
	if !strings.Contains(fields[2], ",") && !strings.Contains(fields[2], ".") {
		return 0, &parseError{"missing milliseconds: " + quote(s)}
	}
	sep := ","
	if strings.Contains(fields[2], ".") {
		sep = "."
	}
	sp := strings.SplitN(fields[2], sep, 2)
	sec, err := strconv.Atoi(sp[0])
	if err != nil || sec < 0 || sec > 59 {
		return 0, &parseError{"bad second: " + quote(s)}
	}
	ms, err := strconv.Atoi(sp[1])
	if err != nil || ms < 0 || ms > 999 {
		return 0, &parseError{"bad millisecond: " + quote(s)}
	}
	if h < 0 {
		return 0, &parseError{"negative hour: " + quote(s)}
	}
	return clock.Millis(int64(h)*3600_000 + int64(m)*60_000 + int64(sec)*1000 + int64(ms)), nil
}

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

func quote(s string) string { return strconv.Quote(strings.TrimSpace(s)) }

// skipToNextRecord consumes lines until a blank separator, used to
// resynchronize after a timing-line failure.
func skipToNextRecord(lr *lineReader) {
	for {
		line, _, _, err := lr.nextLine()
		if err == io.EOF {
			return
		}
		if strings.TrimSpace(line) == "" {
			return
		}
	}
}
