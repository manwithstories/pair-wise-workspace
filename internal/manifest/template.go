package manifest

import (
	"fmt"
	"strconv"
	"strings"
)

type MacroKind uint8

const (
	LiteralMacro MacroKind = iota
	NumberMacro
	TimeMacro
	TimeMSMacro
	RepresentationIDMacro
	BandwidthMacro
)

type Token struct {
	Literal  string
	Kind     MacroKind
	Width    int
	HasWidth bool
	Macro    string
}

type Template struct {
	Raw    string
	Tokens []Token
}

func ParseTemplate(raw string) (*Template, error) {
	if raw == "" {
		return nil, NewError("template.empty", "template is empty")
	}
	t := &Template{Raw: raw}
	for len(raw) > 0 {
		open := strings.IndexByte(raw, '$')
		if open < 0 {
			t.Tokens = append(t.Tokens, Token{Literal: raw})
			break
		}
		if open > 0 {
			t.Tokens = append(t.Tokens, Token{Literal: raw[:open]})
		}
		rest := raw[open+1:]
		close := strings.IndexByte(rest, '$')
		if close < 0 {
			return nil, NewError("template.unclosed_macro", "unclosed macro at byte %d", len(t.Raw)-len(rest)-1)
		}
		token, err := parseMacro(rest[:close])
		if err != nil {
			return nil, err
		}
		t.Tokens = append(t.Tokens, token)
		raw = rest[close+1:]
	}
	if len(t.Tokens) == 0 {
		return nil, NewError("template.empty", "template has no tokens")
	}
	return t, nil
}

func parseMacro(body string) (Token, error) {
	if body == "" {
		return Token{}, NewError("template.empty_macro", "empty macro")
	}
	name := body
	format := ""
	if percent := strings.IndexByte(body, '%'); percent >= 0 {
		name, format = body[:percent], body[percent:]
	}
	token := Token{Macro: body, Width: -1}
	switch name {
	case "Number":
		token.Kind = NumberMacro
	case "Time":
		token.Kind = TimeMacro
	case "TimeMS", "TimeMs":
		token.Kind = TimeMSMacro
	case "RepresentationID":
		token.Kind = RepresentationIDMacro
	case "Bandwidth":
		token.Kind = BandwidthMacro
	default:
		return Token{}, NewError("template.unknown_macro", "unknown macro %q", body)
	}
	if format == "" {
		return token, nil
	}
	if token.Kind == RepresentationIDMacro {
		return Token{}, NewError("template.invalid_width", "RepresentationID does not accept a width")
	}
	width, err := parseDecimalWidth(format)
	if err != nil {
		return Token{}, err
	}
	token.Width, token.HasWidth = width, true
	return token, nil
}

func parseDecimalWidth(format string) (int, error) {
	if len(format) < 3 || format[0] != '%' || format[len(format)-1] != 'd' {
		return 0, NewError("template.invalid_width", "invalid numeric width %q", format)
	}
	digits := format[1 : len(format)-1]
	digits = strings.TrimPrefix(digits, "0")
	if digits == "" {
		return 0, NewError("template.zero_width", "width must be greater than zero")
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, NewError("template.invalid_width", "invalid numeric width %q", format)
		}
	}
	width, err := strconv.Atoi(digits)
	if err != nil || width <= 0 || width > 64 {
		return 0, NewError("template.zero_width", "width must be between 1 and 64")
	}
	return width, nil
}

func (t *Template) Expand(representationID string, bandwidth uint64, number uint64, timeUS int64) (string, error) {
	var b strings.Builder
	b.Grow(len(t.Raw) + 16)
	for _, token := range t.Tokens {
		if token.Kind == LiteralMacro {
			b.WriteString(token.Literal)
			continue
		}
		var value string
		switch token.Kind {
		case NumberMacro:
			value = formatPaddedUint(number, token.Width)
		case TimeMacro:
			if token.HasWidth {
				value = formatPadded(timeUS/1000, token.Width)
			} else {
				value = formatSeconds(timeUS)
			}
		case TimeMSMacro:
			value = formatPadded(timeUS/1000, token.Width)
		case RepresentationIDMacro:
			value = representationID
		case BandwidthMacro:
			value = formatPaddedUint(bandwidth, token.Width)
		}
		b.WriteString(value)
	}
	return b.String(), nil
}

func formatSeconds(us int64) string {
	negative := us < 0
	if negative {
		us = -us
	}
	seconds, millis := us/1_000_000, (us%1_000_000)/1000
	prefix := ""
	if negative {
		prefix = "-"
	}
	return fmt.Sprintf("%s%d.%03d", prefix, seconds, millis)
}

func formatPadded(value int64, width int) string {
	if width <= 0 {
		return strconv.FormatInt(value, 10)
	}
	return fmt.Sprintf("%0*d", width, value)
}

func formatPaddedUint(value uint64, width int) string {
	if width <= 0 {
		return strconv.FormatUint(value, 10)
	}
	return fmt.Sprintf("%0*d", width, value)
}

type ExpandedPath struct {
	RepresentationID string
	SegmentNumber    uint64
	Path             string
}

func ExpandAll(m *Manifest) ([]ExpandedPath, error) {
	t, err := ParseTemplate(m.Template)
	if err != nil {
		return nil, err
	}
	paths := make([]ExpandedPath, 0, len(m.Representations)*64)
	seen := make(map[string]ExpandedPath)
	for _, rep := range m.Representations {
		for _, seg := range rep.Segments {
			path, err := t.Expand(rep.ID, rep.Bandwidth, seg.Number, seg.StartUS)
			if err != nil {
				return nil, err
			}
			expanded := ExpandedPath{RepresentationID: rep.ID, SegmentNumber: seg.Number, Path: path}
			if previous, ok := seen[path]; ok {
				return nil, NewError("template.duplicate_path", "path %q is produced by %s/%d and %s/%d", path, previous.RepresentationID, previous.SegmentNumber, rep.ID, seg.Number)
			}
			seen[path] = expanded
			paths = append(paths, expanded)
		}
	}
	return paths, nil
}
