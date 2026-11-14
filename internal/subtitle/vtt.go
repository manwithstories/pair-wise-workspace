package subtitle

import (
	"fmt"
	"strings"
)

func ParseVTT(data []byte) (*Track, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	endsClean := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	header := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		header = strings.HasPrefix(strings.TrimSpace(line), "WEBVTT")
		break
	}
	if !header {
		return &Track{Format: FormatVTT}, NewParseError("missing_webvtt_header", "WebVTT header not found", 0)
	}
	track := &Track{Format: FormatVTT}
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
		first := strings.TrimSpace(block[0])
		if strings.HasPrefix(first, "WEBVTT") || strings.HasPrefix(first, "NOTE") ||
			strings.HasPrefix(first, "STYLE") || strings.HasPrefix(first, "REGION") {
			continue
		}
		cue, err := parseVTTBlock(block, len(track.Cues)+1)
		if err != nil {
			code := "file_truncated"
			if strings.Contains(err.Error(), "99 hours") {
				code = "timeline_overflow"
			} else if pos == len(lines) && !endsClean {
				code = "file_truncated"
			} else if strings.Contains(err.Error(), "timestamp") {
				code = "invalid_timestamp"
			}
			return track, NewParseError(code, fmt.Sprintf("WebVTT block %d: %v", len(track.Cues)+1, err), len(track.Cues))
		}
		track.Cues = append(track.Cues, cue)
	}
	Normalize(track)
	return track, nil
}

func parseVTTBlock(block []string, index int) (Cue, error) {
	timing := -1
	for i := range block {
		if strings.Contains(block[i], "-->") {
			timing = i
			break
		}
	}
	if timing < 0 {
		return Cue{}, fmt.Errorf("missing timestamp")
	}
	start, end, err := parseArrow(block[timing], ParseVTTTime)
	if err != nil {
		return Cue{}, fmt.Errorf("timestamp: %w", err)
	}
	id := ""
	if timing > 0 {
		id = strings.TrimSpace(block[timing-1])
	}
	return Cue{Index: index, StartMS: start, EndMS: end, ID: id,
		Text: strings.Join(block[timing+1:], "\n")}, nil
}
