// Package manifest parses HLS and DASH manifests into segment
// descriptors carrying full provenance.
package manifest

import (
	"path"
	"strconv"
	"strings"

	"archive/internal/clock"
	"archive/internal/fault"
)

// MaxSegmentBytes caps a single segment read at 64 MiB.
const MaxSegmentBytes = 64 << 20

// maxDurationSecs bounds any single declared duration (anti-overflow).
const maxDurationSecs = 24 * 3600

// Segment describes one media segment with its provenance.
type Segment struct {
	Index  int            // order within the manifest
	URI    string         // normalized, manifest-relative
	Start  clock.Rational // presentation start, seconds
	Dur    clock.Rational // declared duration, seconds
	Source string         // manifest path this segment came from
}

// SafeJoin validates a manifest-relative URI and joins it to base.
// It rejects absolute paths, scheme-qualified URLs and parent escapes.
func SafeJoin(base, uri string) (string, error) {
	if uri == "" {
		return "", fault.Newf(fault.Fatal, "manifest", base, "empty segment URI")
	}
	if strings.Contains(uri, "\\") {
		return "", fault.Newf(fault.Fatal, "manifest", uri, "backslash in URI")
	}
	if strings.HasPrefix(uri, "/") || strings.Contains(uri, "://") {
		return "", fault.Newf(fault.Fatal, "manifest", uri, "absolute URI not allowed")
	}
	c := path.Clean(uri)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fault.Newf(fault.Fatal, "manifest", uri, "path escapes manifest base")
	}
	return path.Join(base, c), nil
}

// parseDecimal parses a non-negative decimal ("2", "2.500") into an
// exact rational number of seconds, rejecting negatives and overflow.
func parseDecimal(s string) (clock.Rational, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return clock.Rational{}, fault.Newf(fault.Fatal, "manifest", s, "negative or invalid duration")
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return clock.Rational{}, fault.Newf(fault.Fatal, "manifest", s, "non-numeric duration")
		}
	}
	if intPart == "" {
		intPart = "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 62)
	if err != nil || whole > maxDurationSecs {
		return clock.Rational{}, fault.Newf(fault.Fatal, "manifest", s, "duration overflow")
	}
	if len(fracPart) > 9 {
		fracPart = fracPart[:9]
	}
	frac, err := strconv.ParseInt("0"+fracPart, 10, 62)
	if err != nil {
		return clock.Rational{}, fault.Newf(fault.Fatal, "manifest", s, "bad fraction")
	}
	den := int64(1)
	for i := 0; i < len(fracPart); i++ {
		den *= 10
	}
	r, err := clock.New(whole*den+frac, den)
	if err != nil {
		return clock.Rational{}, err
	}
	if r.Num > maxDurationSecs*r.Den {
		return clock.Rational{}, fault.Newf(fault.Fatal, "manifest", s, "duration overflow")
	}
	return r, nil
}
