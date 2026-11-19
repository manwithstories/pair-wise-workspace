// Package subtitle parses SRT and WebVTT files into cues carrying byte
// offsets, time ranges and track origin. Overlapping, invalid-time and
// truncated records are isolated as error records; every complete record
// after them is still processed.
package subtitle

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"review/internal/issues"
)

// Cue is one subtitle record on the unified time base.
type Cue struct {
	Track      string  // source track, e.g. "cam00.srt"
	Index      int     // cue number within the file
	ByteOffset int64   // offset of the cue block in the source file
	Start      float64 // seconds
	End        float64 // seconds
	Text       string
}

// Parse sniffs the format (WebVTT vs SRT) and parses accordingly.
func Parse(data []byte, track, path string) ([]Cue, []issues.Issue) {
	if bytes.HasPrefix(bytes.TrimLeft(data, "\xef\xbb\xbf"), []byte("WEBVTT")) {
		return parseVTT(data, track, path)
	}
	return parseSRT(data, track, path)
}

type block struct {
	off   int64
	lines []string
}

// splitBlocks splits data on blank lines, keeping the byte offset of each
// block. The returned bool is false when the file ends mid-record (no
// trailing newline), i.e. the last block is truncated.
func splitBlocks(data []byte) ([]block, bool) {
	var blocks []block
	var cur block
	pos := int64(0)
	clean := true
	for pos < int64(len(data)) {
		nl := bytes.IndexByte(data[pos:], '\n')
		var line []byte
		var next int64
		if nl < 0 {
			line = data[pos:]
			next = int64(len(data))
			clean = false
		} else {
			line = data[pos : pos+int64(nl)]
			next = pos + int64(nl) + 1
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(bytes.TrimSpace(line)) == 0 {
			if len(cur.lines) > 0 {
				blocks = append(blocks, cur)
				cur = block{}
			}
		} else {
			if len(cur.lines) == 0 {
				cur.off = pos
			}
			cur.lines = append(cur.lines, string(line))
		}
		pos = next
	}
	if len(cur.lines) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks, clean
}

var timeRe = regexp.MustCompile(
	`^(\d{1,2}):(\d{2}):(\d{2})[,.](\d{3})\s*-->\s*(\d{1,2}):(\d{2}):(\d{2})[,.](\d{3})(\s.*)?$`)

func parseTimeLine(s string) (start, end float64, err error) {
	m := timeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, fmt.Errorf("bad time range %q", s)
	}
	num := func(i int) float64 { v, _ := strconv.Atoi(m[i]); return float64(v) }
	start = num(1)*3600 + num(2)*60 + num(3) + num(4)/1000
	end = num(5)*3600 + num(6)*60 + num(7) + num(8)/1000
	if end <= start {
		return 0, 0, fmt.Errorf("non-positive duration %q", s)
	}
	return start, end, nil
}

// finish appends a parsed cue, isolating overlaps with the previous cue.
func finish(cues []Cue, iss []issues.Issue, c Cue, path, raw string) ([]Cue, []issues.Issue) {
	if n := len(cues); n > 0 && c.Start < cues[n-1].End-1e-9 {
		return cues, append(iss, issues.Issue{Kind: issues.Overlap, Path: path,
			Track: c.Track, ByteOffset: c.ByteOffset, Raw: raw,
			Msg: fmt.Sprintf("start %.3f overlaps previous cue end %.3f", c.Start, cues[n-1].End)})
	}
	return append(cues, c), iss
}

func badBlock(iss []issues.Issue, path, track string, b block, last, clean bool, err error) []issues.Issue {
	kind := issues.InvalidTime
	if last && !clean {
		kind = issues.TruncatedRecord
	}
	return append(iss, issues.Issue{Kind: kind, Path: path, Track: track,
		ByteOffset: b.off, Raw: strings.Join(b.lines, "\n"), Msg: err.Error()})
}

func parseSRT(data []byte, track, path string) ([]Cue, []issues.Issue) {
	blocks, clean := splitBlocks(data)
	var cues []Cue
	var iss []issues.Issue
	for i, b := range blocks {
		last := i == len(blocks)-1
		if len(b.lines) < 2 {
			iss = badBlock(iss, path, track, b, last, clean,
				fmt.Errorf("incomplete record: want index and time lines"))
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(b.lines[0])); err != nil {
			iss = append(iss, issues.Issue{Kind: issues.MalformedRecord, Path: path,
				Track: track, ByteOffset: b.off, Raw: strings.Join(b.lines, "\n"),
				Msg: "index line is not a number"})
			continue
		}
		start, end, err := parseTimeLine(b.lines[1])
		if err != nil {
			iss = badBlock(iss, path, track, b, last, clean, err)
			continue
		}
		cues, iss = finish(cues, iss, Cue{Track: track, Index: len(cues) + 1,
			ByteOffset: b.off, Start: start, End: end,
			Text: strings.Join(b.lines[2:], "\n")}, path, strings.Join(b.lines, "\n"))
	}
	return cues, iss
}

func parseVTT(data []byte, track, path string) ([]Cue, []issues.Issue) {
	blocks, clean := splitBlocks(data)
	var cues []Cue
	var iss []issues.Issue
	for i, b := range blocks {
		last := i == len(blocks)-1
		head := strings.TrimPrefix(b.lines[0], "\xef\xbb\xbf")
		if i == 0 && strings.HasPrefix(head, "WEBVTT") {
			continue // file header
		}
		if strings.HasPrefix(head, "NOTE") {
			continue
		}
		timeIdx := 0
		if !strings.Contains(b.lines[0], "-->") {
			if len(b.lines) < 2 {
				iss = badBlock(iss, path, track, b, last, clean,
					fmt.Errorf("incomplete cue: want time line"))
				continue
			}
			timeIdx = 1 // cue identifier line
		}
		if timeIdx >= len(b.lines) {
			iss = badBlock(iss, path, track, b, last, clean,
				fmt.Errorf("incomplete cue: want time line"))
			continue
		}
		start, end, err := parseTimeLine(b.lines[timeIdx])
		if err != nil {
			iss = badBlock(iss, path, track, b, last, clean, err)
			continue
		}
		cues, iss = finish(cues, iss, Cue{Track: track, Index: len(cues) + 1,
			ByteOffset: b.off, Start: start, End: end,
			Text: strings.Join(b.lines[timeIdx+1:], "\n")}, path, strings.Join(b.lines, "\n"))
	}
	return cues, iss
}
