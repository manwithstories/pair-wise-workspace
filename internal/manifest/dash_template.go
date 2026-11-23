package manifest

import (
	"strconv"
	"strings"
)

// expandTemplateString substitutes the DASH $...$ identifiers used in segment
// templates. Width formatting such as $Number%05d$ is supported.
func expandTemplateString(tmpl string, number, bandwidth int64, repID string) string {
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '$' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '$')
		if end < 0 {
			b.WriteString(tmpl[i:])
			break
		}
		token := tmpl[i+1 : i+1+end]
		i += end + 2
		b.WriteString(applyToken(token, number, bandwidth, repID))
	}
	return b.String()
}

func applyToken(token string, number, bandwidth int64, repID string) string {
	name, format := token, ""
	if p := strings.IndexByte(token, '%'); p >= 0 {
		name, format = token[:p], token[p+1:]
	}
	var val string
	switch name {
	case "Number", "Time":
		val = strconv.FormatInt(number, 10)
	case "Bandwidth":
		val = strconv.FormatInt(bandwidth, 10)
	case "RepresentationID":
		val = repID
	case "":
		return "$"
	default:
		return "$" + token + "$"
	}
	if format == "" {
		return val
	}
	// format is like "05d" or "05x".
	verb := format[len(format)-1]
	width := 0
	if len(format) > 1 {
		width, _ = strconv.Atoi(strings.TrimLeft(format[:len(format)-1], "0"))
	}
	if verb == 'd' && width > len(val) {
		return strings.Repeat("0", width-len(val)) + val
	}
	return val
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
