package subtitle

import (
	"fmt"
	"strconv"
	"strings"
)

func ParseSRT(data []byte) (*Track, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	endsClean := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	track := &Track{Format: FormatSRT}
	for pos := 0; pos < len(lines); {
		for pos < len(lines) && strings.TrimSpace(lines[pos]) == "" {
			pos++
		}
		if pos >= len(lines) {
			break
		}
		start := pos
		for pos < len(lines) && strings.TrimSpace(lines[pos]) != "" {
			pos++
		}
		block := lines[start:pos]
		cue, err := parseSRTBlock(block, len(track.Cues)+1)
		if err != nil {
			code := "file_truncated"
			if strings.Contains(err.Error(), "99 hours") {
				code = "timeline_overflow"
			} else if pos == len(lines) && !endsClean {
				code = "file_truncated"
			} else if strings.Contains(err.Error(), "timestamp") {
				code = "invalid_timestamp"
			}
			return track, NewParseError(code, fmt.Sprintf("SRT block %d: %v", len(track.Cues)+1, err), len(track.Cues))
		}
		track.Cues = append(track.Cues, cue)
	}
	Normalize(track)
	return track, nil
}

func parseSRTBlock(block []string, fallbackIndex int) (Cue, error) {
	if len(block) < 2 {
		return Cue{}, fmt.Errorf("incomplete cue")
	}
	timeLine := 0
	index := fallbackIndex
	if !strings.Contains(block[0], "-->") {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(block[0], "\ufeff")))
		if err != nil {
			return Cue{}, fmt.Errorf("missing timestamp")
		}
		index = n
		timeLine = 1
	}
	if timeLine >= len(block) {
		return Cue{}, fmt.Errorf("missing timestamp")
	}
	start, end, err := parseArrow(block[timeLine], ParseSRTTime)
	if err != nil {
		return Cue{}, fmt.Errorf("timestamp: %w", err)
	}
	return Cue{Index: index, StartMS: start, EndMS: end, Text: strings.Join(block[timeLine+1:], "\n")}, nil
}

func parseArrow(line string, parse func(string) (int64, error)) (int64, int64, error) {
	parts := strings.SplitN(line, "-->", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("missing -->")
	}
	left := strings.TrimSpace(parts[0])
	rightFields := strings.Fields(strings.TrimSpace(parts[1]))
	if len(rightFields) == 0 {
		return 0, 0, fmt.Errorf("missing end timestamp")
	}
	start, err := parse(left)
	if err != nil {
		return 0, 0, err
	}
	end, err := parse(rightFields[0])
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}
