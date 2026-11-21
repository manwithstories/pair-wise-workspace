package captions

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mediacheck/internal/fault"
	"mediacheck/internal/timeline"
)

// MaxCueBytes bounds a single subtitle block to 1 MiB.
const MaxCueBytes = 1 << 20

// Cue is one subtitle block on the shared nanosecond time base.
type Cue struct {
	Seq      int    `json:"seq"`
	Start    int64  `json:"start_ns"`
	End      int64  `json:"end_ns"`
	RawStart string `json:"raw_start"` // original time text, preserved
	RawEnd   string `json:"raw_end"`
	Text     string `json:"text"`
	Offset   int64  `json:"offset"` // byte offset of the block in the source
}

// Result carries parsed cues plus every recoverable fault found;
// recoverable errors are never silently dropped.
type Result struct {
	Cues   []Cue       `json:"cues"`
	Faults *fault.List `json:"faults"`
}

// parseTimestamp parses "HH:MM:SS,mmm" / "HH:MM:SS.mmm" and the VTT
// short form "MM:SS.mmm" into nanoseconds. srcOffset locates errors.
func parseTimestamp(s string, srcOffset int64) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.Replace(s, ",", ".", 1)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fault.New(fault.CatCaption, srcOffset, true, "malformed timestamp %q", s)
	}
	secParts := strings.SplitN(parts[len(parts)-1], ".", 2)
	nums := make([]int64, 0, 3)
	for _, p := range append(parts[:len(parts)-1], secParts[0]) {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil || n < 0 {
			return 0, fault.New(fault.CatCaption, srcOffset, true, "bad number in timestamp %q", s)
		}
		nums = append(nums, n)
	}
	var hh, mm, ss int64
	if len(nums) == 3 {
		hh, mm, ss = nums[0], nums[1], nums[2]
	} else {
		mm, ss = nums[0], nums[1]
	}
	if mm > 59 || ss > 59 {
		return 0, fault.New(fault.CatCaption, srcOffset, true, "minute/second out of range in %q", s)
	}
	ms := int64(0)
	if len(secParts) == 2 {
		frac := secParts[1]
		if len(frac) > 3 {
			return 0, fault.New(fault.CatCaption, srcOffset, true, "millisecond field too long in %q", s)
		}
		for len(frac) < 3 {
			frac += "0"
		}
		v, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fault.New(fault.CatCaption, srcOffset, true, "bad milliseconds in %q", s)
		}
		ms = v
	}
	ns := ((hh*60+mm)*60+ss)*timeline.NanosPerSecond + ms*1_000_000
	if ns > timeline.MaxSpanNanos {
		return 0, fault.New(fault.CatCaption, srcOffset, true,
			"timestamp %q beyond 48h timeline bound", s)
	}
	return ns, nil
}

// checkCue validates one cue's times, appending recoverable faults for
// reversed or out-of-range times; the cue itself is kept.
func checkCue(res *Result, c *Cue) {
	if c.End <= c.Start {
		res.Faults.Addf(fault.CatCaption, c.Offset, true,
			"reversed cue time: %s --> %s", c.RawStart, c.RawEnd)
	}
	if err := timeline.ValidateSpan(c.Start, c.End, c.Offset); err != nil {
		res.Faults.Add(err)
	}
}

// blockReader splits a subtitle file into blank-line-separated blocks,
// tracking each block's byte offset and enforcing the 1 MiB block cap.
type block struct {
	text   string
	offset int64
}

func readBlocks(r io.Reader, res *Result) []block {
	br := bufio.NewReaderSize(r, 64*1024)
	var blocks []block
	var cur strings.Builder
	var curOff, off int64 = -1, 0
	flush := func() {
		if cur.Len() > 0 {
			blocks = append(blocks, block{cur.String(), curOff})
			cur.Reset()
			curOff = -1
		}
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if curOff < 0 {
				curOff = off
			}
			if cur.Len()+len(line) > MaxCueBytes {
				res.Faults.Addf(fault.CatCaption, curOff, true,
					"subtitle block exceeds 1 MiB cap, truncated")
				flush()
				continue
			}
			if trimmed == "" {
				flush()
			} else {
				cur.WriteString(line)
			}
			off += int64(len(line))
		}
		if err != nil {
			flush()
			return blocks
		}
	}
}

var _ = fmt.Sprintf

// ParseSRT parses SubRip subtitle blocks:
//
//	<seq>
//	HH:MM:SS,mmm --> HH:MM:SS,mmm
//	text...
//
// Truncated trailing blocks (missing time line) are reported as
// recoverable faults; sequence numbers and raw time text are kept.
func ParseSRT(r io.Reader) *Result {
	res := &Result{Faults: &fault.List{}}
	for _, b := range readBlocks(r, res) {
		lines := strings.Split(strings.TrimRight(b.text, "\r\n"), "\n")
		if len(lines) < 2 {
			res.Faults.Addf(fault.CatCaption, b.offset, true,
				"truncated SRT block: %q", firstLine(lines))
			continue
		}
		seq, err := strconv.Atoi(strings.TrimSpace(lines[0]))
		if err != nil {
			res.Faults.Addf(fault.CatCaption, b.offset, true,
				"bad SRT sequence number %q", strings.TrimSpace(lines[0]))
			continue
		}
		c, ok := parseTimeLine(lines[1], b.offset, res)
		if !ok {
			continue
		}
		c.Seq = seq
		c.Offset = b.offset
		if len(lines) > 2 {
			c.Text = strings.Join(lines[2:], "\n")
		}
		checkCue(res, &c)
		res.Cues = append(res.Cues, c)
	}
	return res
}

func firstLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

// parseTimeLine parses "start --> end" into a Cue with raw text kept.
func parseTimeLine(line string, offset int64, res *Result) (Cue, bool) {
	parts := strings.SplitN(line, "-->", 2)
	if len(parts) != 2 {
		res.Faults.Addf(fault.CatCaption, offset, true,
			"truncated block, missing time line: %q", strings.TrimSpace(line))
		return Cue{}, false
	}
	rawStart := strings.TrimSpace(parts[0])
	// VTT may append settings after the end time; take first token.
	endFields := strings.Fields(strings.TrimSpace(parts[1]))
	rawEnd := ""
	if len(endFields) > 0 {
		rawEnd = endFields[0]
	}
	start, err := parseTimestamp(rawStart, offset)
	if err != nil {
		res.Faults.Add(err)
		return Cue{}, false
	}
	end, err := parseTimestamp(rawEnd, offset)
	if err != nil {
		res.Faults.Add(err)
		return Cue{}, false
	}
	return Cue{Start: start, End: end, RawStart: rawStart, RawEnd: rawEnd}, true
}
