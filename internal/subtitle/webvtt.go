package subtitle

import (
	"strings"

	"mediasync/internal/media"
)

// ParseWebVTT parses WebVTT. NOTE/STYLE/REGION blocks and cue settings are
// accepted; malformed cue blocks are reported by offset and skipped.
func ParseWebVTT(data []byte, file string) *Result {
	res := &Result{Format: "webvtt", Bytes: len(data)}
	if len(data) > MaxSubtitleBytes {
		res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputWebVTT,
			media.CodeLimitExceeded, file, 0, 0, "input is %d bytes, limit is %d", len(data), MaxSubtitleBytes).AsFatal())
		return res
	}
	lines := splitLines(data)
	res.Lines = len(lines)
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0].text), "WEBVTT") {
		res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputWebVTT,
			media.CodeMalformed, file, 0, 0, "missing WEBVTT signature line").AsFatal())
		return res
	}
	p := &parser{res: res, lines: lines, input: media.InputWebVTT, file: file, stage: media.StageSubtitle}
	for _, b := range splitBlocks(lines) {
		if b.first == 0 {
			continue // signature line
		}
		first := strings.ToUpper(strings.TrimSpace(lines[b.first].text))
		if strings.HasPrefix(first, "NOTE") || first == "STYLE" || first == "REGION" {
			continue
		}
		if !hasArrow(lines, b) {
			if isNumericIdentifier(first) {
				res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputWebVTT, media.CodeTruncated,
					file, lines[b.first].off, len(res.Cues)+1,
					"cue identifier %q at line %d has no timing line", strings.TrimSpace(lines[b.first].text), lines[b.first].num))
			} else {
				res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputWebVTT, media.CodeMalformed,
					file, lines[b.first].off, len(res.Cues)+1,
					"cue block at line %d has no timing line", lines[b.first].num))
			}
			continue
		}
		if p.tooMany() {
			break
		}
		cue, ok := p.cueFromBlock(b, true)
		if ok {
			res.Cues = append(res.Cues, cue)
		}
	}
	if len(res.Cues) == 0 && len(res.Issues) == 0 {
		res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputWebVTT,
			media.CodeMalformed, file, 0, 0, "no subtitle cues found"))
	}
	return res
}

// Detect picks a parser from the file extension and then from content.
func Detect(data []byte, name string) *Result {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".vtt"), strings.HasSuffix(lower, ".webvtt"):
		return ParseWebVTT(data, name)
	case strings.HasSuffix(lower, ".srt"):
		return ParseSRT(data, name)
	}
	if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(string(data), "\uFEFF")), "WEBVTT") {
		return ParseWebVTT(data, name)
	}
	return ParseSRT(data, name)
}
