package captions

import (
	"io"
	"strconv"
	"strings"

	"mediacheck/internal/fault"
)

// ParseVTT parses WebVTT: a WEBVTT header, then cue blocks with an
// optional identifier line and "start --> end" timing. Cues map onto
// the same nanosecond base as SRT. Missing header, truncated blocks
// and bad times are recoverable faults, never silently dropped.
func ParseVTT(r io.Reader) *Result {
	res := &Result{Faults: &fault.List{}}
	blocks := readBlocks(r, res)
	if len(blocks) == 0 {
		res.Faults.Addf(fault.CatCaption, 0, false, "empty WebVTT input")
		return res
	}
	// First block must be the WEBVTT header.
	hdr := strings.TrimSpace(firstLine(strings.Split(blocks[0].text, "\n")))
	if !strings.HasPrefix(hdr, "WEBVTT") {
		res.Faults.Addf(fault.CatCaption, blocks[0].offset, false,
			"missing WEBVTT header, got %q", hdr)
		return res
	}
	seq := 0
	for _, b := range blocks[1:] {
		lines := strings.Split(strings.TrimRight(b.text, "\r\n"), "\n")
		if len(lines) == 0 {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(lines[0]), "NOTE") {
			continue // comment block
		}
		timeIdx := 0
		if !strings.Contains(lines[0], "-->") {
			if len(lines) < 2 {
				res.Faults.Addf(fault.CatCaption, b.offset, true,
					"truncated VTT block after identifier %q", strings.TrimSpace(lines[0]))
				continue
			}
			timeIdx = 1 // identifier line present
		}
		c, ok := parseTimeLine(lines[timeIdx], b.offset, res)
		if !ok {
			continue
		}
		seq++
		c.Seq = seq
		c.Offset = b.offset
		if len(lines) > timeIdx+1 {
			c.Text = strings.Join(lines[timeIdx+1:], "\n")
		}
		checkCue(res, &c)
		res.Cues = append(res.Cues, c)
	}
	return res
}

var _ = strconv.Itoa
