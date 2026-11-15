package parse

import (
	"fmt"
	"strings"

	"subtool/internal/model"
)

// ParseVTT parses WebVTT. The WEBVTT header and NOTE/STYLE/REGION
// blocks are preserved in Header; cue identifiers are kept when unique.
func ParseVTT(text string) *Result {
	lines := NormalizeLines(text)
	res := &Result{Header: map[string]string{}}
	i := 0
	if len(lines) > 0 && strings.HasPrefix(lines[0], "WEBVTT") {
		res.Header["WEBVTT"] = strings.TrimSpace(strings.TrimPrefix(lines[0], "WEBVTT"))
		i++
	}
	seq := 0
	for i < len(lines) {
		if strings.TrimSpace(lines[i]) == "" {
			i++
			continue
		}
		// Collect one block.
		blockLine := i + 1
		var blk []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			blk = append(blk, lines[i])
			i++
		}
		first := strings.TrimSpace(blk[0])
		upper := strings.ToUpper(first)
		switch {
		case strings.HasPrefix(upper, "NOTE"):
			res.Header["NOTE"] = strings.Join(blk, "\n")
			continue
		case strings.HasPrefix(upper, "STYLE"):
			res.Header["STYLE"] = strings.Join(blk[1:], "\n")
			continue
		case strings.HasPrefix(upper, "REGION"):
			res.Header["REGION"] = strings.Join(blk[1:], "\n")
			continue
		}
		// Optional cue identifier line before the timing line.
		id := ""
		timingIdx := 0
		if !strings.Contains(first, "-->") && len(blk) > 1 {
			id = first
			timingIdx = 1
		}
		start, end, _, err := SplitTiming(strings.TrimSpace(blk[timingIdx]))
		if err != nil {
			res.Issues = append(res.Issues, model.Issue{
				Line: blockLine, Code: model.CodeParse,
				Msg: fmt.Sprintf("unrecognized cue block: %q", first),
			})
			continue
		}
		sRat, errS := ParseTimecode(start)
		eRat, errE := ParseTimecode(end)
		if errS != nil || errE != nil {
			res.Issues = append(res.Issues, model.Issue{
				Line: blockLine + timingIdx, Code: model.CodeBadTimecode,
				Msg: "bad cue timing",
			})
			continue
		}
		seq++
		if id == "" {
			id = fmt.Sprintf("%04d", seq)
		}
		text := strings.Join(blk[timingIdx+1:], "\n")
		c := model.Cue{
			ID: id, Start: sRat, End: eRat, Text: text,
			Tags: ExtractTags(text), SourceLine: blockLine,
		}
		if issue := ValidateCue(&c); issue != nil {
			res.Issues = append(res.Issues, *issue)
			continue
		}
		res.Cues = append(res.Cues, c)
	}
	return res
}
