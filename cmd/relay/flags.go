package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseBytes accepts a plain integer byte count or a suffixed form (KiB, MiB).
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "MiB"):
		multiplier = 1 << 20
		s = s[:len(s)-3]
	case strings.HasSuffix(s, "KiB"):
		multiplier = 1 << 10
		s = s[:len(s)-3]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte count %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("byte count must be positive")
	}
	return n * multiplier, nil
}

func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("integer must be non-negative")
	}
	return n, nil
}
