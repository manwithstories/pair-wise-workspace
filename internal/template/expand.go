// Package template expands $-placeholders in media/initialization URL
// templates: $Number$, $Time$, $RepresentationID$, $Bandwidth$, with $$
// escaping and %0Nd width control.
package template

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dashkit/internal/model"
)

// Values carries the per-segment substitution values.
type Values struct {
	Number           uint64
	Time             uint64
	RepresentationID string
	Bandwidth        int64
}

var phRe = regexp.MustCompile(`\$([A-Za-z]+)(?:%0(\d+)d)?\$`)

// Expand substitutes placeholders in tpl. "$$" yields a literal "$".
// Unknown placeholders and malformed "$" sequences are errors.
func Expand(tpl string, v Values) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tpl); {
		c := tpl[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(tpl) && tpl[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		m := phRe.FindStringSubmatchIndex(tpl[i:])
		if m == nil || m[0] != 0 {
			return "", fmt.Errorf("%s: malformed placeholder at offset %d in %q",
				model.CodeUnknownHolder, i, tpl)
		}
		name := tpl[i+m[2] : i+m[3]]
		width := 0
		if m[4] >= 0 {
			width, _ = strconv.Atoi(tpl[i+m[4] : i+m[5]])
		}
		var out string
		switch name {
		case "Number":
			out = formatUint(v.Number, width)
		case "Time":
			out = formatUint(v.Time, width)
		case "RepresentationID":
			out = v.RepresentationID
		case "Bandwidth":
			out = formatUint(uint64(v.Bandwidth), width)
		default:
			return "", fmt.Errorf("%s: unknown placeholder $%s$", model.CodeUnknownHolder, name)
		}
		b.WriteString(out)
		i += m[1]
	}
	return b.String(), nil
}

func formatUint(n uint64, width int) string {
	if width > 0 {
		return fmt.Sprintf("%0*d", width, n)
	}
	return strconv.FormatUint(n, 10)
}

// ExpandSegments expands one representation's timeline into media URLs.
// Segment numbers must increase strictly without duplicates or holes.
func ExpandSegments(rep *model.Representation) ([]string, []model.Segment, []model.Problem) {
	tpl := rep.Template
	segs, probs := model.ExpandTimeline(tpl.Timeline, tpl.StartNumber)
	var urls []string
	for i, s := range segs {
		if i > 0 && s.Number != segs[i-1].Number+1 {
			probs = append(probs, model.Problem{Code: model.CodeNumberGap, Rep: rep.ID,
				Msg: fmt.Sprintf("segment number %d follows %d", s.Number, segs[i-1].Number)})
		}
		u, err := Expand(tpl.Media, Values{
			Number: s.Number, Time: s.T,
			RepresentationID: rep.ID, Bandwidth: rep.Bandwidth,
		})
		if err != nil {
			probs = append(probs, model.Problem{Code: model.CodeUnknownHolder, Rep: rep.ID, Msg: err.Error()})
			continue
		}
		urls = append(urls, u)
	}
	return urls, segs, probs
}
