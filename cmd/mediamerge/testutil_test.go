package main

import (
	"strconv"
	"strings"
	"testing"
)

// field returns the first whitespace-delimited token on the line beginning
// with prefix.
func field(t *testing.T, text, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			rest := strings.TrimSpace(line[len(prefix):])
			if i := strings.IndexAny(rest, " \t("); i >= 0 {
				return rest[:i]
			}
			return rest
		}
	}
	t.Fatalf("no line with prefix %q in:\n%s", prefix, text)
	return ""
}

// parseField parses an integer field.
func parseField(t *testing.T, text, prefix string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(field(t, text, prefix), 10, 64)
	if err != nil {
		t.Fatalf("field %q is not an integer: %v", prefix, err)
	}
	return v
}

// parseFraction parses a "N/M" field.
func parseFraction(t *testing.T, text, prefix string) (int, int) {
	t.Helper()
	parts := strings.Split(field(t, text, prefix), "/")
	if len(parts) != 2 {
		t.Fatalf("field %q is not a fraction", prefix)
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("fraction %q is not numeric", prefix)
	}
	return a, b
}
