package subtitle

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"mediasync/internal/clock"
)

// ParseSRT parses an SRT stream. Invalid times, truncated blocks and
// overlapping records are reported with their source line and skipped;
// duplicate timestamps are kept and marked ambiguous.
func ParseSRT(r io.Reader, tb clock.Timebase) *Report {
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
	// Flush one block: lines[0]=index, lines[1]=timing, rest=text.
	idx := 0
	flush := func(block []string, nos []int) {
		if len(block) == 0 {
			return
		}
		if len(block) < 2 {
			a.reject(nos[0], "truncated block")
			return
		}
		if _, err := strconv.Atoi(strings.TrimSpace(block[0])); err != nil {
			a.reject(nos[0], "bad index line")
			return
		}
		startRaw, endRaw, err := splitTiming(block[1])
		if err != nil {
			a.reject(nos[1], "bad timing: "+err.Error())
			return
		}
		startMs, err1 := parseTimestamp(startRaw, ',')
		endMs, err2 := parseTimestamp(endRaw, ',')
		if err1 != nil || err2 != nil {
			msg := ""
			if err1 != nil {
				msg = err1.Error()
			} else {
				msg = err2.Error()
			}
			a.reject(nos[1], "invalid time: "+msg)
			return
		}
		start, end := tb.MillisToFrames(startMs), tb.MillisToFrames(endMs)
		if end <= start {
			a.reject(nos[1], "end not after start")
			return
		}
		idx++
		a.add(Record{
			Index: idx, Start: start, End: end,
			StartRaw: startRaw, EndRaw: endRaw,
			Text: strings.Join(block[2:], "\n"),
			Source: "srt", Line: nos[1],
		})
	}
	var block []string
	var nos []int
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			flush(block, nos)
			block, nos = nil, nil
			continue
		}
		block = append(block, ln)
		nos = append(nos, lineNos[i])
	}
	flush(block, nos)
	return &a.report
}
