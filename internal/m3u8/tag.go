package m3u8

import (
	"fmt"
	"strconv"
	"strings"
)

// LineKind classifies a single playlist line.
type LineKind int

const (
	KindBlank LineKind = iota
	KindComment
	KindTag
	KindURI
)

// Attr is a single KEY=VALUE attribute list entry.
type Attr struct {
	Key   string
	Value string
}

// Line is the lexical form of one playlist line.
type Line struct {
	Kind  LineKind
	Tag   string // tag name without the leading '#'
	Value string // text after the first ':' for tags
	Text  string // raw text for comments and URIs
}

// ParseLine lexes one raw playlist line.
func ParseLine(raw string) Line {
	s := strings.TrimRight(raw, "\r")
	switch {
	case s == "":
		return Line{Kind: KindBlank}
	case strings.HasPrefix(s, "#EXT"):
		tag, value := s, ""
		if i := strings.Index(s, ":"); i >= 0 {
			tag, value = s[:i], s[i+1:]
		}
		return Line{Kind: KindTag, Tag: strings.TrimPrefix(tag, "#"), Value: value}
	case strings.HasPrefix(s, "#"):
		return Line{Kind: KindComment, Text: s}
	default:
		return Line{Kind: KindURI, Text: s}
	}
}

// String serializes the line back to its strict textual form.
func (l Line) String() string {
	if l.Kind == KindTag {
		if l.Value == "" {
			return "#" + l.Tag
		}
		return "#" + l.Tag + ":" + l.Value
	}
	return l.Text
}

// ParseAttrs parses a comma-separated attribute list, honoring quoted strings.
func ParseAttrs(s string) ([]Attr, error) {
	var attrs []Attr
	for _, part := range splitAttrs(s) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("m3u8: malformed attribute %q", part)
		}
		attrs = append(attrs, Attr{Key: strings.TrimSpace(kv[0]), Value: strings.TrimSpace(kv[1])})
	}
	return attrs, nil
}

func splitAttrs(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// FormatAttrs serializes an attribute list back to a single line body.
func FormatAttrs(attrs []Attr) string {
	parts := make([]string, len(attrs))
	for i, a := range attrs {
		parts[i] = a.Key + "=" + a.Value
	}
	return strings.Join(parts, ",")
}

// Quote renders a quoted-string attribute value.
func Quote(s string) string { return strconv.Quote(s) }

// Unquote strips quotes from a quoted-string attribute value.
func Unquote(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return strings.Trim(s, "\"")
}
