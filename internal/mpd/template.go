package mpd

import (
	"fmt"
	"strconv"
	"strings"
)

// Addressing modes supported in SegmentTemplate@media.
const (
	AddrNumber = "number"
	AddrTime   = "time"
)

// TemplateVars carries the values substituted into media template
// placeholders for one segment of one representation.
type TemplateVars struct {
	RepresentationID string
	Bandwidth        int64
	Number           int64
	Time             int64
}

// AddressingMode reports whether a media template addresses segments by
// $Number$ or $Time$. Empty string means no recognised placeholder.
func AddressingMode(media string) string {
	if strings.Contains(media, "$Number") {
		return AddrNumber
	}
	if strings.Contains(media, "$Time") {
		return AddrTime
	}
	return ""
}

// ExpandTemplate expands $$ escapes and the DASH identifiers
// $RepresentationID$, $Bandwidth$, $Number$ and $Time$, each optionally
// followed by a printf width format such as %05d.
func ExpandTemplate(tpl string, v TemplateVars) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tpl); {
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
		j := i + 1
		for j < len(tpl) && tpl[j] != '$' {
			j++
		}
		if j >= len(tpl) {
			return "", fmt.Errorf("mpd: unterminated placeholder in %q", tpl)
		}
		inner := tpl[i+1 : j]
		i = j + 1
		name, format := inner, ""
		if k := strings.IndexByte(inner, '%'); k >= 0 {
			name, format = inner[:k], inner[k:]
		}
		switch name {
		case "RepresentationID":
			if format != "" {
				return "", fmt.Errorf("mpd: format not allowed on $%s$", name)
			}
			b.WriteString(v.RepresentationID)
		case "Bandwidth":
			if err := writeFormatted(&b, format, v.Bandwidth); err != nil {
				return "", err
			}
		case "Number":
			if err := writeFormatted(&b, format, v.Number); err != nil {
				return "", err
			}
		case "Time":
			if err := writeFormatted(&b, format, v.Time); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("mpd: unknown placeholder $%s$", name)
		}
	}
	return b.String(), nil
}

func writeFormatted(b *strings.Builder, format string, val int64) error {
	if format == "" {
		b.WriteString(strconv.FormatInt(val, 10))
		return nil
	}
	if !validIntFormat(format) {
		return fmt.Errorf("mpd: invalid number format %q", format)
	}
	fmt.Fprintf(b, format, val)
	return nil
}

// validIntFormat accepts only "%Nd" / "%0Nd" width formats.
func validIntFormat(f string) bool {
	if len(f) < 2 || f[0] != '%' || f[len(f)-1] != 'd' {
		return false
	}
	for _, c := range f[1 : len(f)-1] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
