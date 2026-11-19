package timeline

import (
	"sort"

	"mediasync/internal/media"
)

// TCRecord is one timecode label mapped onto the unwrapped timeline.
type TCRecord struct {
	Index     int    `json:"index"`
	Label     string `json:"label"`
	Frames    int64  `json:"frame_number"`
	Unwrapped int64  `json:"unwrapped_frames"`
	NS        int64  `json:"time_ns"`
	Wrapped   bool   `json:"day_rollover"`
	Duplicate bool   `json:"duplicate"`
	Offset    int64  `json:"offset"`
}

// TCStats summarises a timecode stream.
type TCStats struct {
	Labels    int `json:"labels"`
	Accepted  int `json:"accepted"`
	Wrapped   int `json:"day_rollovers"`
	Duplicate int `json:"duplicate_timestamps"`
	Rejected  int `json:"rejected"`
}

// TCResult is the parsed stream plus the errors that could not be converted.
type TCResult struct {
	Rate    Rate           `json:"rate"`
	Drop    bool           `json:"drop_frame"`
	Records []TCRecord     `json:"records"`
	Issues  []*media.Issue `json:"issues"`
	Stats   TCStats        `json:"stats"`
}

// ParseTimecodeStream parses newline-separated labels in order, resolving day
// rollover and recording repeated timestamps. Labels that cannot be converted
// are reported with their byte offset in the joined input and skipped; the
// rest of the stream is still converted.
func ParseTimecodeStream(data []byte, rate Rate) *TCResult {
	res := &TCResult{Rate: rate}
	unwrap := NewUnwrapper(rate)
	var offset int64
	index := 0
	for len(data) > 0 {
		end := 0
		for end < len(data) && data[end] != '\n' && data[end] != '\r' {
			end++
		}
		line := string(data[:end])
		lineOffset := offset
		// Advance past the line and its terminator.
		skip := end
		if skip < len(data) && data[skip] == '\r' {
			skip++
		}
		if skip < len(data) && data[skip] == '\n' {
			skip++
		}
		data = data[skip:]
		offset += int64(skip)
		if len(trimSpace(line)) == 0 {
			continue
		}
		index++
		res.Stats.Labels++
		tc, err := ParseTimecode(line, rate)
		if err != nil {
			res.Stats.Rejected++
			res.Issues = append(res.Issues, media.New(media.StageTimebase, media.InputTC,
				media.CodeInvalidTimestamp, "%s", err.(*media.Issue).Message).
				WithOffset(lineOffset).WithRecord(index))
			continue
		}
		unwrapped, wrapped := unwrap.Unwrap(tc.Frames)
		rec := TCRecord{
			Index:     index,
			Label:     line,
			Frames:    tc.Frames,
			Unwrapped: unwrapped,
			NS:        rate.FramesToNanos(unwrapped),
			Wrapped:   wrapped,
			Offset:    lineOffset,
		}
		if n := len(res.Records); n > 0 && unwrapped == res.Records[n-1].Unwrapped {
			rec.Duplicate = true
			res.Stats.Duplicate++
		}
		if wrapped {
			res.Stats.Wrapped++
		}
		if tc.Drop {
			res.Drop = true
		}
		res.Stats.Accepted++
		res.Records = append(res.Records, rec)
	}
	sort.SliceStable(res.Records, func(i, j int) bool {
		return res.Records[i].Index < res.Records[j].Index
	})
	return res
}

// Trimmed reports whether any record required day-rollover expansion.
func (r *TCResult) Trimmed() bool { return r.Stats.Wrapped > 0 }

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
