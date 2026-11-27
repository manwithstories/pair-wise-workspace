package caption

import (
	"bytes"
	"io"
	"strconv"
	"strings"

	"avcore/internal/adapter"
	"avcore/internal/clock"
)

// ParseWebVTT streams a WebVTT document. It skips NOTE/STYLE/REGION blocks,
// accepts optional cue identifiers and the MM:SS.mmm short timestamp form,
// and records recoverable failures per cue exactly like the SRT parser.
func ParseWebVTT(r io.Reader, maxBytes int64) Result {
	res := Result{Format: FormatWebVTT}
	lr := newLineReader(r)

	// Header: the first line must start with WEBVTT.
	first, firstNo, firstOff, ferr := lr.nextLine()
	if ferr != nil && ferr != io.EOF {
		res.Fatal = adapter.NewError(adapter.CodeInternal, "caption read failure: "+ferr.Error(),
			adapter.Source{Line: firstNo, Offset: firstOff})
		return res
	}
	if !strings.HasPrefix(strings.TrimPrefix(first, "\uFEFF"), "WEBVTT") {
		res.Fatal = adapter.NewError(adapter.CodeUnsupported,
			"not a WebVTT document (missing WEBVTT signature)",
			adapter.Source{Line: firstNo, Offset: firstOff})
		return res
	}

	var cueSeq int
	for {
		line, lineNo, lineOff, err := lr.nextLine()
		if err == io.EOF {
			return res
		}
		if err != nil {
			res.Fatal = adapter.NewError(adapter.CodeInternal, "caption read failure: "+err.Error(),
				adapter.Source{Line: lineNo, Offset: lineOff})
			return res
		}
		if lr.offsetOf() > maxBytes {
			res.Fatal = adapter.NewError(adapter.CodeOversize,
				"caption stream exceeds size limit", adapter.Source{})
			return res
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Block-level metadata: NOTE, STYLE and REGION run to the next blank line.
		if strings.HasPrefix(trimmed, "NOTE") || trimmed == "STYLE" ||
			strings.HasPrefix(trimmed, "STYLE ") || trimmed == "REGION" ||
			strings.HasPrefix(trimmed, "REGION ") {
			skipToNextRecord(lr)
			continue
		}
		// A line without "-->" is an optional cue identifier; the timing
		// line follows it.
		timingLine, tNo, tOff := line, lineNo, lineOff
		if !strings.Contains(line, "-->") {
			next, nNo, nOff, nerr := lr.nextLine()
			if nerr == io.EOF {
				res.BadRecords = append(res.BadRecords, bad(adapter.CodeTruncated,
					"cue identifier without timing line", nNo, nOff))
				return res
			}
			timingLine, tNo, tOff = next, nNo, nOff
		}
		start, end, terr := parseVTTRange(timingLine)
		if terr != nil {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeBadTimestamp,
				terr.Error(), tNo, tOff))
			skipToNextRecord(lr)
			continue
		}
		var text bytes.Buffer
		textStart := lr.offsetOf()
		truncated := false
		for {
			txt, _, _, rerr := lr.nextLine()
			if strings.TrimSpace(txt) == "" {
				if rerr == io.EOF {
					truncated = true
				}
				break
			}
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(txt)
			if rerr == io.EOF {
				truncated = true
				break
			}
		}
		if truncated && text.Len() == 0 {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeTruncated,
				"cue has no text payload", tNo, textStart))
			continue
		}
		cueSeq++
		cue := Cue{
			Index:      cueSeq,
			StartTicks: start,
			EndTicks:   end,
			Text:       text.String(),
			SourceLine: lineNo,
		}
		if len(res.Cues) > 0 && start < res.Cues[len(res.Cues)-1].EndTicks {
			res.BadRecords = append(res.BadRecords, bad(adapter.CodeOverlap,
				"cue overlaps previous cue", lineNo, lineOff))
		}
		res.Cues = append(res.Cues, cue)
	}
}

// parseVTTRange parses "start --> end", dropping any cue settings after end.
func parseVTTRange(line string) (int64, int64, error) {
	parts := strings.SplitN(line, "-->", 2)
	if len(parts) != 2 {
		return 0, 0, &parseError{"missing --> separator"}
	}
	start, err := parseVTTTime(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	rest := strings.TrimSpace(parts[1])
	// Cue settings (line:, position:, ...) follow the end timestamp.
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		rest = rest[:sp]
	}
	end, err := parseVTTTime(rest)
	if err != nil {
		return 0, 0, err
	}
	if end <= start {
		return 0, 0, &parseError{"cue end must be after start"}
	}
	return start, end, nil
}

// parseVTTTime accepts HH:MM:SS.mmm and MM:SS.mmm; hours may exceed 23.
func parseVTTTime(s string) (int64, error) {
	fields := strings.Split(s, ":")
	if len(fields) != 2 && len(fields) != 3 {
		return 0, &parseError{"bad VTT timestamp: " + quote(s)}
	}
	var h, m int
	var secField string
	if len(fields) == 3 {
		v, err := strconv.Atoi(fields[0])
		if err != nil || v < 0 {
			return 0, &parseError{"bad hour: " + quote(s)}
		}
		h = v
		m, err = strconv.Atoi(fields[1])
		if err != nil || m < 0 || m > 59 {
			return 0, &parseError{"bad minute: " + quote(s)}
		}
		secField = fields[2]
	} else {
		v, err := strconv.Atoi(fields[0])
		if err != nil || v < 0 || v > 59 {
			return 0, &parseError{"bad minute: " + quote(s)}
		}
		m = v
		secField = fields[1]
	}
	sp := strings.SplitN(secField, ".", 2)
	if len(sp) != 2 {
		return 0, &parseError{"VTT timestamps require .mmm: " + quote(s)}
	}
	sec, err := strconv.Atoi(sp[0])
	if err != nil || sec < 0 || sec > 59 {
		return 0, &parseError{"bad second: " + quote(s)}
	}
	ms, err := strconv.Atoi(sp[1])
	if err != nil || ms < 0 || ms > 999 {
		return 0, &parseError{"bad millisecond: " + quote(s)}
	}
	return clock.Millis(int64(h)*3600_000 + int64(m)*60_000 + int64(sec)*1000 + int64(ms)), nil
}
