package parse

import (
	"fmt"
	"strconv"
	"strings"

	"subtool/internal/model"
)

// ParseSRT parses SubRip text. It tolerates out-of-order or duplicate
// indices, mixed newlines (already normalized) and missing blank lines
// between blocks by falling back to timing-line detection.
func ParseSRT(text string) *Result {
	lines := NormalizeLines(text)
	res := &Result{Header: map[string]string{}}
	seq := 0
	usedIDs := map[string]bool{}
	emit := func(c model.Cue) {
		if issue := ValidateCue(&c); issue != nil {
			res.Issues = append(res.Issues, *issue)
			return
		}
		res.Cues = append(res.Cues, c)
	}
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		blockLine := i + 1
		timing := line
		idx := -1
		// Optional numeric index line; used as the cue ID when unique,
		// so positional SRT numbering survives quarantine round-trips.
		if n, err := strconv.Atoi(line); err == nil && n >= 0 && i+1 < len(lines) {
			idx = n
			timing = strings.TrimSpace(lines[i+1])
			i++
		}
		start, end, _, err := SplitTiming(timing)
		if err != nil {
			res.Issues = append(res.Issues, model.Issue{
				Line: blockLine, Code: model.CodeParse,
				Msg: fmt.Sprintf("unrecognized block: %q", line),
			})
			i++
			continue
		}
		i++
		var body []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			body = append(body, lines[i])
			i++
		}
		sRat, errS := ParseTimecode(start)
		eRat, errE := ParseTimecode(end)
		if errS != nil || errE != nil {
			msg := ""
			if errS != nil {
				msg = errS.Error()
			} else {
				msg = errE.Error()
			}
			res.Issues = append(res.Issues, model.Issue{
				Line: blockLine, Code: model.CodeBadTimecode, Msg: msg,
			})
			continue
		}
		seq++
		id := fmt.Sprintf("%04d", seq)
		if idx > 0 && !usedIDs[fmt.Sprintf("%04d", idx)] {
			id = fmt.Sprintf("%04d", idx)
		}
		for usedIDs[id] {
			seq++
			id = fmt.Sprintf("%04d", seq)
		}
		usedIDs[id] = true
		text := strings.Join(body, "\n")
		emit(model.Cue{
			ID:         id,
			Start:      sRat,
			End:        eRat,
			Text:       text,
			Tags:       ExtractTags(text),
			SourceLine: blockLine,
		})
	}
	return res
}
