package clock

import (
	"fmt"
	"strings"
)

// Timecode is a parsed HH:MM:SS:FF timecode on a supported rate.
type Timecode struct {
	Rate       Rate
	H, M, S, F int64
}

// ParseTimecode parses "HH:MM:SS:FF" against rate r. It rejects
// malformed fields and frame numbers outside the rate's nominal range.
func ParseTimecode(s string, r Rate) (Timecode, error) {
	if !r.Valid() {
		return Timecode{}, fmt.Errorf("clock: unsupported rate %s", r)
	}
	parts := strings.Split(s, ":")
	if len(parts) != 4 {
		return Timecode{}, fmt.Errorf("clock: malformed timecode %q", s)
	}
	var v [4]int64
	for i, p := range parts {
		if len(p) != 2 {
			return Timecode{}, fmt.Errorf("clock: malformed timecode %q", s)
		}
		n, err := digits(p)
		if err != nil {
			return Timecode{}, fmt.Errorf("clock: malformed timecode %q", s)
		}
		v[i] = n
	}
	tc := Timecode{Rate: r, H: v[0], M: v[1], S: v[2], F: v[3]}
	if tc.M > 59 || tc.S > 59 {
		return Timecode{}, fmt.Errorf("clock: timecode %q field out of range", s)
	}
	if tc.F >= r.NominalFPS() {
		return Timecode{}, fmt.Errorf("clock: illegal frame number %d for rate %s", tc.F, r)
	}
	return tc, nil
}

// Frame returns the absolute frame index of the timecode.
func (tc Timecode) Frame() int64 {
	return ((tc.H*60+tc.M)*60+tc.S)*tc.Rate.NominalFPS() + tc.F
}

// Nanos maps the timecode onto the unified time base.
func (tc Timecode) Nanos() int64 { return tc.Rate.FrameToNanos(tc.Frame()) }

func digits(s string) (int64, error) {
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("clock: %q is not numeric", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
