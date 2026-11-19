// Package timeline merges subtitle tracks and device timecode lists into
// fixed-size block tasks on the unified time base, keeping the source
// mapping (track + byte offset) at every block boundary.
package timeline

import (
	"fmt"
	"strconv"
	"strings"

	"review/internal/clock"
	"review/internal/issues"
	"review/internal/subtitle"
)

// CueSpan is a subtitle cue projected onto frame numbers.
type CueSpan struct {
	Track              string
	ByteOffset         int64
	StartFrame, EndFrame int64
}

// TCRecord is one device timecode entry mapped to the absolute timeline.
type TCRecord struct {
	Seq        int
	Raw        string // original text, kept for unconvertible records
	AbsFrame   int64
	ByteOffset int64
}

// Block is one chunk of work. Sources let a reviewer trace every frame in
// the block back to the source segment it came from.
type Block struct {
	Cam                int
	Index              int
	StartFrame, EndFrame int64
	Spans              []CueSpan
	Timecodes          []TCRecord
	Notes              []string
}

// ParseTimecodeList parses a CSV timecode list ("seq,HH:MM:SS:FF" per line,
// ";" for drop-frame). When dayWrap is on, a backward jump past midnight
// increments the day counter. Unconvertible records are kept verbatim as
// located error records.
func ParseTimecodeList(data []byte, path string, r clock.Rate, dayWrap bool) ([]TCRecord, []issues.Issue) {
	var recs []TCRecord
	var iss []issues.Issue
	lines := strings.Split(string(data), "\n")
	off := int64(0)
	day := 0
	prevInDay := int64(-1)
	for i, line := range lines {
		raw := strings.TrimRight(line, "\r")
		curOff := off
		off += int64(len(line)) + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, ",", 2)
		if len(parts) != 2 {
			iss = append(iss, issues.Issue{Kind: issues.TCUnconvertible, Path: path,
				ByteOffset: curOff, Line: i + 1, Raw: raw, Msg: "want seq,timecode"})
			continue
		}
		tc, err := clock.ParseTimecode(parts[1])
		if err != nil {
			iss = append(iss, issues.Issue{Kind: issues.TCUnconvertible, Path: path,
				ByteOffset: curOff, Line: i + 1, Raw: raw, Msg: err.Error()})
			continue
		}
		inDay, err := clock.Timecode{HH: tc.HH, MM: tc.MM, SS: tc.SS, FF: tc.FF, Drop: tc.Drop}.ToFrames(r)
		if err != nil {
			iss = append(iss, issues.Issue{Kind: issues.TCUnconvertible, Path: path,
				ByteOffset: curOff, Line: i + 1, Raw: raw, Msg: err.Error()})
			continue
		}
		inDay %= r.FramesPerDay(tc.Drop)
		if dayWrap && prevInDay >= 0 && r.DayWraps(prevInDay, inDay, tc.Drop) {
			day++
		}
		prevInDay = inDay
		tc.Day = day
		abs, err := tc.ToFrames(r)
		if err != nil {
			iss = append(iss, issues.Issue{Kind: issues.TCUnconvertible, Path: path,
				ByteOffset: curOff, Line: i + 1, Raw: raw, Msg: err.Error()})
			continue
		}
		seq, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		recs = append(recs, TCRecord{Seq: seq, Raw: raw, AbsFrame: abs, ByteOffset: curOff})
	}
	return recs, iss
}

// BuildBlocks cuts one camera's timeline into blockFrames-sized tasks and
// attaches every cue span and timecode record intersecting each block.
// Boundary anomalies (cues spanning a cut, day wraps, missing timecode
// frames) are preserved as notes on the block.
func BuildBlocks(cam int, totalFrames, blockFrames int64, cues []subtitle.Cue, tcs []TCRecord, r clock.Rate) []Block {
	var spans []CueSpan
	for _, c := range cues {
		spans = append(spans, CueSpan{Track: c.Track, ByteOffset: c.ByteOffset,
			StartFrame: r.SecondsToFrames(c.Start), EndFrame: r.SecondsToFrames(c.End)})
	}
	var blocks []Block
	for start := int64(0); start < totalFrames; start += blockFrames {
		end := start + blockFrames
		if end > totalFrames {
			end = totalFrames
		}
		b := Block{Cam: cam, Index: len(blocks), StartFrame: start, EndFrame: end}
		for _, sp := range spans {
			if sp.StartFrame < end && sp.EndFrame > start {
				b.Spans = append(b.Spans, sp)
				if sp.StartFrame < start || sp.EndFrame > end {
					b.Notes = append(b.Notes, fmt.Sprintf("cue %s@%d spans block boundary", sp.Track, sp.ByteOffset))
				}
			}
		}
		for i, tc := range tcs {
			if tc.AbsFrame >= start && tc.AbsFrame < end {
				b.Timecodes = append(b.Timecodes, tc)
				if i > 0 {
					if gap := tc.AbsFrame - tcs[i-1].AbsFrame; gap > r.Nominal()*2 {
						b.Notes = append(b.Notes, fmt.Sprintf("missing timecode frames before seq %d (gap %d)", tc.Seq, gap))
					}
					if tc.AbsFrame < tcs[i-1].AbsFrame {
						b.Notes = append(b.Notes, fmt.Sprintf("timecode day wrap at seq %d", tc.Seq))
					}
				}
			}
		}
		blocks = append(blocks, b)
	}
	return blocks
}
