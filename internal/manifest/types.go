// Package manifest parses HLS and DASH manifests into a shared track
// model. When an index is incomplete, segment boundaries are derived from
// a verifiable segment duration and flagged with derived confidence
// instead of being silently trusted.
package manifest

import (
	"strconv"
	"strings"

	"mediamap/internal/clock"
)

// Confidence levels for segment boundaries.
const (
	Indexed = "indexed" // boundaries came from an explicit index
	Derived = "derived" // boundaries inferred from a declared duration
)

// SegmentRef is one segment on the unified 90 kHz timeline.
type SegmentRef struct {
	URI      string `json:"uri"`
	Start    int64  `json:"start_ticks"`
	Duration int64  `json:"duration_ticks"`
	Derived  bool   `json:"derived,omitempty"`
}

// Representation is one bitrate rung of a track.
type Representation struct {
	ID        string `json:"id"`
	Bandwidth int    `json:"bandwidth"`
	// StartOffset is the declared presentation start (90 kHz ticks),
	// from EXT-X-START or a DASH presentation offset; 0 when absent.
	StartOffset int64        `json:"start_offset_ticks"`
	Segments    []SegmentRef `json:"segments"`
}

// Track is one isolated media track: a ladder of representations.
type Track struct {
	Name            string           `json:"name"`
	Kind            string           `json:"kind"` // "video" or "audio"
	Timescale       int64            `json:"timescale"`
	Confidence      string           `json:"confidence"`
	Representations []Representation `json:"representations"`
}

// parseDecimal parses a non-negative decimal ("4", "4.000", "3.75") into
// an exact rational number of seconds, avoiding float accumulation.
func parseDecimal(s string) (clock.Rational, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return clock.Rational{}, err
	}
	num, den := w, int64(1)
	if frac != "" {
		for _, c := range frac {
			if c < '0' || c > '9' {
				return clock.Rational{}, strconv.ErrSyntax
			}
		}
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return clock.Rational{}, err
		}
		den = 1
		for range frac {
			den *= 10
		}
		num = w*den + f
	}
	if neg {
		num = -num
	}
	return clock.NewRational(num, den), nil
}

// secondsToTicks converts a rational number of seconds to 90 kHz ticks,
// rounding once at the end.
func secondsToTicks(r clock.Rational) int64 {
	return clock.NewRational(r.Num*clock.MediaTimebase, r.Den).Ticks()
}
