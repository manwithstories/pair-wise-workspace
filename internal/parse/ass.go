package parse

import (
	"fmt"
	"strings"

	"subtool/internal/model"
)

// ParseASS parses SSA/ASS scripts. Style and info header blocks are
// preserved verbatim in Header; Dialogue events become cues. ASS
// timestamps use centisecond precision ("H:MM:SS.cc").
func ParseASS(text string) *Result {
	lines := NormalizeLines(text)
	res := &Result{Header: map[string]string{}}
	section := ""
	format := []string{"Layer", "Start", "End", "Style", "Name", "MarginL", "MarginR", "MarginV", "Effect", "Text"}
	var headers []string
	seq := 0
	for idx, raw := range lines {
		line := strings.TrimSpace(raw)
		lineNo := idx + 1
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			headers = append(headers, line)
			continue
		}
		if section != "Events" {
			headers = append(headers, raw)
			continue
		}
		if strings.HasPrefix(line, "Format:") {
			format = nil
			for _, f := range strings.Split(strings.TrimPrefix(line, "Format:"), ",") {
				format = append(format, strings.TrimSpace(f))
			}
			headers = append(headers, raw)
			continue
		}
		if !strings.HasPrefix(line, "Dialogue:") {
			headers = append(headers, raw)
			continue
		}
		fields := strings.SplitN(strings.TrimPrefix(line, "Dialogue:"), ",", len(format))
		if len(fields) < len(format) {
			res.Issues = append(res.Issues, model.Issue{
				Line: lineNo, Code: model.CodeMissing,
				Msg: "Dialogue line has fewer fields than Format",
			})
			continue
		}
		get := func(name string) string {
			for i, f := range format {
				if strings.EqualFold(f, name) {
					return strings.TrimSpace(fields[i])
				}
			}
			return ""
		}
		sRat, errS := ParseTimecode(get("Start"))
		eRat, errE := ParseTimecode(get("End"))
		if errS != nil || errE != nil {
			res.Issues = append(res.Issues, model.Issue{
				Line: lineNo, Code: model.CodeBadTimecode, Msg: "bad Dialogue timing",
			})
			continue
		}
		seq++
		layer := 0
		fmt.Sscanf(get("Layer"), "%d", &layer)
		// ASS hard line breaks round-trip back to real newlines.
		text := strings.ReplaceAll(get("Text"), `\N`, "\n")
		c := model.Cue{
			ID: fmt.Sprintf("%04d", seq), Start: sRat, End: eRat,
			Text: text, Tags: ExtractTags(text), Layer: layer,
			SourceLine: lineNo,
		}
		if issue := ValidateCue(&c); issue != nil {
			res.Issues = append(res.Issues, *issue)
			continue
		}
		res.Cues = append(res.Cues, c)
	}
	res.Header["ASS"] = strings.Join(headers, "\n")
	return res
}
