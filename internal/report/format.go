package report

import (
	"strconv"
	"strings"
)

type field struct{ k, v string }

func joinFields(fs []field) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		if f.v == "" {
			parts[i] = f.k
		} else {
			parts[i] = f.k + "=" + f.v
		}
	}
	return strings.Join(parts, " ")
}

func itoa(v int) string { return strconv.Itoa(v) }

func formatBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

func formatRate(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func shortHash(h string) string {
	const n = 12
	if len(h) <= n {
		return h
	}
	return h[:n]
}
