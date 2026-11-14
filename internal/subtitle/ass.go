package subtitle

import (
	"fmt"
	"strconv"
	"strings"
)

var defaultASSFormat = []string{
	"layer", "start", "end", "style", "name", "marginl", "marginr", "marginv", "effect", "text",
}

func ParseASS(data []byte) (*Track, error) {
	raw := strings.TrimPrefix(string(data), "\ufeff")
	endsClean := strings.HasSuffix(raw, "\n")
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	track := &Track{Format: FormatASS}
	inEvents := false
	var format []string
	for pos, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inEvents = strings.EqualFold(trimmed, "[Events]")
			continue
		}
		if !inEvents || trimmed == "" || strings.HasPrefix(trimmed, ";") {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "format:") {
			format = splitASSFields(trimmed[len("format:"):])
			if len(format) < 8 {
				return track, NewParseError("dialogue_fields_short", "Events Format has fewer than eight fields", len(track.Cues))
			}
			continue
		}
		if !strings.HasPrefix(lower, "dialogue:") {
			continue
		}
		fields := format
		if len(fields) == 0 {
			fields = defaultASSFormat
		}
		cue, err := parseASSDialogue(trimmed[len("dialogue:"):], fields, len(track.Cues)+1)
		if err != nil {
			code := "dialogue_fields_short"
			switch {
			case strings.Contains(err.Error(), "timeline_overflow"):
				code = "timeline_overflow"
			case strings.Contains(err.Error(), "invalid_timestamp"):
				code = "invalid_timestamp"
			case pos == len(lines)-1 && !endsClean && strings.Contains(err.Error(), "incomplete"):
				code = "file_truncated"
			}
			return track, NewParseError(code, err.Error(), len(track.Cues))
		}
		track.Cues = append(track.Cues, cue)
	}
	if !hasEventSection(lines) {
		return track, NewParseError("missing_events_section", "ASS file has no [Events] section", len(track.Cues))
	}
	Normalize(track)
	return track, nil
}

func hasEventSection(lines []string) bool {
	for _, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), "[Events]") {
			return true
		}
	}
	return false
}

func splitASSFields(value string) []string {
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(parts[i]))
	}
	return parts
}

func parseASSDialogue(payload string, format []string, index int) (Cue, error) {
	if len(format) < 8 {
		return Cue{}, fmt.Errorf("Events Format has fewer than eight fields")
	}
	fields := strings.SplitN(strings.TrimSpace(payload), ",", len(format))
	if len(fields) < 8 || len(fields) < len(format) {
		return Cue{}, fmt.Errorf("expected %d fields, got %d; incomplete", len(format), len(fields))
	}
	get := func(name string) string {
		for i, field := range format {
			if field == name && i < len(fields) {
				return fields[i]
			}
		}
		return ""
	}
	startField, endField := get("start"), get("end")
	if startField == "" || endField == "" {
		return Cue{}, fmt.Errorf("missing Start or End field")
	}
	start, err := ParseASSTime(startField)
	if err != nil {
		if err == ErrTimelineOverflow {
			return Cue{}, fmt.Errorf("timeline_overflow: start %s", startField)
		}
		return Cue{}, fmt.Errorf("invalid_timestamp: start %q", startField)
	}
	end, err := ParseASSTime(endField)
	if err != nil {
		if err == ErrTimelineOverflow {
			return Cue{}, fmt.Errorf("timeline_overflow: end %s", endField)
		}
		return Cue{}, fmt.Errorf("invalid_timestamp: end %q", endField)
	}
	layer := strings.TrimSpace(get("layer"))
	if layer == "" {
		layer = strings.TrimSpace(get("marked"))
	}
	if _, err := strconv.Atoi(layer); err != nil && layer != "" {
		layer = "0"
	}
	text := unescapeASSText(get("text"))
	return Cue{
		Index: index, StartMS: start, EndMS: end, Text: text,
		Layer: layer, Style: strings.TrimSpace(get("style")), Name: strings.TrimSpace(get("name")),
		MarginL: strings.TrimSpace(get("marginl")), MarginR: strings.TrimSpace(get("marginr")),
		MarginV: strings.TrimSpace(get("marginv")), Effect: strings.TrimSpace(get("effect")),
	}, nil
}

func unescapeASSText(text string) string {
	text = strings.ReplaceAll(text, `\N`, "\n")
	return strings.ReplaceAll(text, `\n`, "\n")
}
