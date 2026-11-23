package manifest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var iso8601Dur = regexp.MustCompile(`^P(?:([0-9]+)D)?(?:T(?:([0-9]+)H)?(?:([0-9]+)M)?(?:([0-9]+(?:\.[0-9]+)?)S)?)?$`)

// parseISO8601Duration converts a DASH duration string to nanoseconds.
func parseISO8601Duration(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := iso8601Dur.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%w: bad duration %q", ErrInvalid, s)
	}
	var ns float64
	if m[1] != "" {
		d, _ := strconv.ParseFloat(m[1], 64)
		ns += d * 86400
	}
	if m[2] != "" {
		h, _ := strconv.ParseFloat(m[2], 64)
		ns += h * 3600
	}
	if m[3] != "" {
		mi, _ := strconv.ParseFloat(m[3], 64)
		ns += mi * 60
	}
	if m[4] != "" {
		sec, _ := strconv.ParseFloat(m[4], 64)
		ns += sec
	}
	if ns <= 0 {
		return 0, fmt.Errorf("%w: non-positive duration %q", ErrInvalid, s)
	}
	return int64(ns * 1e9), nil
}
