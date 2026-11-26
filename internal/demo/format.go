package demo

import (
	"strconv"
	"strings"
)

func itoa(v int) string { return strconv.Itoa(v) }

func joinFloats(values []float64, precision int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.FormatFloat(v, 'f', precision, 64)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
