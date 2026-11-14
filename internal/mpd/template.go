package mpd

import (
	"fmt"
	"strconv"
	"strings"
)

// Vars carries the values a segment template can reference.
type Vars struct {
	RepresentationID string
	Number           uint64
	Time             uint64
	Bandwidth        uint64
}

// TemplateError reports a template problem with 1-based line and column.
type TemplateError struct {
	Line int
	Col  int
	Msg  string
}

func (e *TemplateError) Error() string {
	return fmt.Sprintf("template error at line %d, column %d: %s", e.Line, e.Col, e.Msg)
}

// knownVars lists the supported $...$ variables.
var knownVars = map[string]bool{
	"RepresentationID": true,
	"Number":           true,
	"Time":             true,
	"Bandwidth":        true,
}

// numericVars lists variables that accept a %0Nd width format.
var numericVars = map[string]bool{
	"Number": true,
	"Time":   true,
}

// Expand renders a segment template. Supported forms are $RepresentationID$,
// $Number%05d$ (any %0<width>d), $Time$, $Bandwidth$ and the $$ escape.
// Unknown variables or malformed formats return a *TemplateError with the
// line and column of the opening '$'.
func Expand(tpl string, v Vars) (string, error) {
	var b strings.Builder
	b.Grow(len(tpl) + 16)
	i := 0
	for i < len(tpl) {
		if tpl[i] != '$' {
			b.WriteByte(tpl[i])
			i++
			continue
		}
		if i+1 < len(tpl) && tpl[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		line, col := lineCol(tpl, i)
		end := strings.IndexByte(tpl[i+1:], '$')
		if end < 0 {
			return "", &TemplateError{Line: line, Col: col, Msg: "unterminated $...$ variable"}
		}
		expr := tpl[i+1 : i+1+end]
		name, format := expr, ""
		if pct := strings.IndexByte(expr, '%'); pct >= 0 {
			name, format = expr[:pct], expr[pct:]
		}
		if !knownVars[name] {
			return "", &TemplateError{Line: line, Col: col, Msg: fmt.Sprintf("unknown variable $%s$", name)}
		}
		out, err := renderVar(name, format, v)
		if err != nil {
			return "", &TemplateError{Line: line, Col: col, Msg: err.Error()}
		}
		b.WriteString(out)
		i += end + 2
	}
	return b.String(), nil
}

func renderVar(name, format string, v Vars) (string, error) {
	if format != "" {
		if !numericVars[name] {
			return "", fmt.Errorf("format %q not allowed for $%s$", format, name)
		}
		width, err := parseWidth(format)
		if err != nil {
			return "", err
		}
		num := v.Number
		if name == "Time" {
			num = v.Time
		}
		return fmt.Sprintf("%0*d", width, num), nil
	}
	switch name {
	case "RepresentationID":
		return v.RepresentationID, nil
	case "Number":
		return strconv.FormatUint(v.Number, 10), nil
	case "Time":
		return strconv.FormatUint(v.Time, 10), nil
	case "Bandwidth":
		return strconv.FormatUint(v.Bandwidth, 10), nil
	}
	return "", fmt.Errorf("unknown variable $%s$", name)
}

// parseWidth validates a %0<width>d format and returns the width.
func parseWidth(format string) (int, error) {
	if len(format) < 4 || format[0] != '%' || format[1] != '0' || format[len(format)-1] != 'd' {
		return 0, fmt.Errorf("invalid format %q: want %%0<width>d", format)
	}
	width, err := strconv.Atoi(format[2 : len(format)-1])
	if err != nil || width < 1 || width > 20 {
		return 0, fmt.Errorf("invalid format width %q: want 1..20", format)
	}
	return width, nil
}

// lineCol converts a byte offset into 1-based line and column numbers.
func lineCol(s string, off int) (int, int) {
	line, col := 1, 1
	for i := 0; i < off && i < len(s); i++ {
		if s[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}
