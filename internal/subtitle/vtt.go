package subtitle

import (
	"bufio"
	"io"
	"strings"

	"mediasync/internal/clock"
)

// ParseVTT parses a WebVTT stream. A missing WEBVTT header makes the whole
// input unrecoverable; bad cue timings, truncated cues and overlaps are
// reported per source line and skipped.
func ParseVTT(r io.Reader, tb clock.Timebase) *Report {
	a := newAccum()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	var lineNos []int
	n := 0
	for sc.Scan() {
		n++
		lines = append(lines, sc.Text())
		lineNos = append(lineNos, n)
	}
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "WEBVTT") {
		a.reject(1, "missing WEBVTT header")
		return &a.report
	}
	idx := 0
	flush := func(block []string, nos []int) {
		if len(block) == 0 {
			return
		}
		// Skip NOTE/STYLE/REGION blocks.
		first := strings.TrimSpace(block[0])
		if strings.HasPrefix(first, "NOTE") || first == "STYLE" || first == "REGION" {
			return
		}
		timingAt := 0
		if !strings.Contains(block[0], "-->") {
			if len(block) < 2 {
				a.reject(nos[0], "truncated cue")
				return
			}
			timingAt = 1 // cue identifier line
		}
		if len(block) <= timingAt {
			a.reject(nos[0], "truncated cue")
			return
		}
		startRaw, endRaw, err := splitTiming(block[timingAt])
		if err != nil {
			a.reject(nos[timingAt], "bad timing: "+err.Error())
			return
		}
		startMs, err1 := parseTimestamp(startRaw, '.')
		endMs, err2 := parseTimestamp(endRaw, '.')
		if err1 != nil || err2 != nil {
			msg := ""
			if err1 != nil {
				msg = err1.Error()
			} else {
				msg = err2.Error()
			}
			a.reject(nos[timingAt], "invalid time: "+msg)
			return
		}
		start, end := tb.MillisToFrames(startMs), tb.MillisToFrames(endMs)
		if end <= start {
			a.reject(nos[timingAt], "end not after start")
			return
		}
		idx++
		a.add(Record{
			Index: idx, Start: start, End: end,
			StartRaw: startRaw, EndRaw: endRaw,
			Text: strings.Join(block[timingAt+1:], "\n"),
			Source: "vtt", Line: nos[timingAt],
		})
	}
	var block []string
	var nos []int
	for i, ln := range lines[1:] {
		if strings.TrimSpace(ln) == "" {
			flush(block, nos)
			block, nos = nil, nil
			continue
		}
		block = append(block, ln)
		nos = append(nos, lineNos[i+1])
	}
	flush(block, nos)
	return &a.report
}
