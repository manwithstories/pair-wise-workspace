package subtitle

import (
	"strconv"
	"strings"

	"mediasync/internal/media"
)

// ParseSRT parses SubRip text. Malformed records are reported with their raw
// byte offsets and skipped; later well-formed records are still returned.
func ParseSRT(data []byte, file string) *Result {
	res := &Result{Format: "srt", Bytes: len(data)}
	if len(data) > MaxSubtitleBytes {
		res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputSRT,
			media.CodeLimitExceeded, file, 0, 0, "input is %d bytes, limit is %d", len(data), MaxSubtitleBytes).AsFatal())
		return res
	}
	lines := splitLines(data)
	res.Lines = len(lines)
	p := &parser{res: res, lines: lines, input: media.InputSRT, file: file, stage: media.StageSubtitle}
	for _, b := range splitBlocks(lines) {
		if isNumericIdentifier(lines[b.first].text) && !hasArrow(lines, b) {
			// An index line with no timing line: the record is truncated.
			res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputSRT, media.CodeTruncated,
				file, lines[b.first].off, len(res.Cues)+1,
				"subtitle index %q at line %d has no timing line", strings.TrimSpace(lines[b.first].text), lines[b.first].num))
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
		res.Issues = append(res.Issues, newIssue(media.StageSubtitle, media.InputSRT,
			media.CodeMalformed, file, 0, 0, "no subtitle cues found"))
	}
	return res
}

func hasArrow(lines []rawLine, b block) bool {
	for k := b.first; k <= b.last; k++ {
		if strings.Contains(lines[k].text, "-->") {
			return true
		}
	}
	return false
}

// parseIndex validates an SRT index token; a non-positive index is reported.
func parseIndex(tok string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(tok))
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, strconv.ErrRange
	}
	return v, nil
}
