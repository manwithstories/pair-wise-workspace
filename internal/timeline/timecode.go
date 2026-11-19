package timeline

import (
	"fmt"
	"strconv"
	"strings"

	"mediasync/internal/media"
)

// Timecode is a parsed SMPTE/EBU timecode label plus the frame number it
// denotes, before day-rollover unwrapping.
type Timecode struct {
	Label  string `json:"label"`
	Hours  int64  `json:"hours"`
	Minute int64  `json:"minutes"`
	Second int64  `json:"seconds"`
	Frame  int64  `json:"frames"`
	Frames int64  `json:"frame_number"`
	Drop   bool   `json:"drop_frame"`
}

// ParseTimecode parses labels of the form HH:MM:SS:FF (non-drop) or
// HH:MM:SS;FF (drop-frame). The rate determines the frames-per-second base
// and whether drop-frame labels are legal.
func ParseTimecode(label string, rate Rate) (Timecode, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return Timecode{}, tcErr("empty timecode label")
	}
	if err := rate.Validate(); err != nil {
		return Timecode{}, tcErr("%v", err)
	}
	// Drop-frame labels separate hours/minutes/seconds with ':' and the
	// frame field with ';', matching SMPTE HB:MM:SS;FF.
	sep := byte(':')
	if n := strings.Count(label, ";"); n > 1 {
		return Timecode{}, tcErr("malformed timecode %q: multiple ';'", label)
	} else if n == 1 {
		sep = ';'
	}
	var parts []string
	if sep == ';' {
		head, frames, ok := strings.Cut(label, ";")
		if !ok {
			return Timecode{}, tcErr("malformed timecode %q: missing ';' frame separator", label)
		}
		parts = append(strings.Split(head, ":"), frames)
	} else {
		parts = strings.Split(label, ":")
	}
	if len(parts) != 4 {
		return Timecode{}, tcErr("malformed timecode %q: want HH:MM:SS%cFF", label, sep)
	}
	var vals [4]int64
	for i, p := range parts {
		if p == "" || len(p) > 3 {
			return Timecode{}, tcErr("malformed timecode %q: bad field %d", label, i+1)
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v < 0 {
			return Timecode{}, tcErr("malformed timecode %q: field %d is not a number", label, i+1)
		}
		vals[i] = v
	}
	tc := Timecode{Label: label, Hours: vals[0], Minute: vals[1], Second: vals[2], Frame: vals[3], Drop: sep == ';'}
	if tc.Minute > 59 || tc.Second > 59 {
		return Timecode{}, tcErr("timecode %q out of range: minutes/seconds must be 0..59", label)
	}
	if tc.Hours > 99 {
		return Timecode{}, tcErr("timecode %q out of range: hours must be 0..99", label)
	}
	nominal := rate.Nominal()
	if tc.Frame >= nominal {
		return Timecode{}, tcErr("timecode %q out of range: frame %d >= %d", label, tc.Frame, nominal)
	}
	if tc.Drop && nominal != 30 && nominal != 60 {
		return Timecode{}, tcErr("drop-frame timecode %q requires 29.97 or 59.94 fps, got %s", label, rate)
	}
	if tc.Drop && tc.Frame < (nominal/30)*2 && tc.Second == 0 && tc.Minute%10 != 0 {
		return Timecode{}, tcErr("timecode %q labels a dropped frame", label)
	}
	total := ((tc.Hours*3600+tc.Minute*60+tc.Second)*nominal + tc.Frame)
	if tc.Drop {
		drop := nominal / 30 * 2
		minutes := tc.Hours*60 + tc.Minute
		total -= drop * (minutes - minutes/10)
	}
	if total < 0 {
		return Timecode{}, tcErr("timecode %q yields negative frame number", label)
	}
	tc.Frames = total
	return tc, nil
}

func tcErr(format string, args ...any) error {
	return media.New(media.StageTimebase, media.InputTC, media.CodeInvalidTimestamp, format, args...)
}

// DayFrames returns how many frames one 24-hour timecode day spans.
func DayFrames(rate Rate) int64 {
	return rate.Nominal() * 86400
}

// Unwrapper resolves day-boundary rollover for timecode frame numbers.
// Repeated timestamps are returned unchanged; only large backwards jumps
// (more than half a day) are treated as a forward rollover.
type Unwrapper struct {
	Rate     Rate
	Day      int64
	prev     int64
	seen     bool
	Rollover int
}

// NewUnwrapper builds an unwrapper for the given rate.
func NewUnwrapper(rate Rate) *Unwrapper {
	return &Unwrapper{Rate: rate, Day: DayFrames(rate)}
}

// Unwrap maps an observed frame number onto a monotonically growing timeline.
// wrapped reports that a day boundary was crossed by this record.
func (u *Unwrapper) Unwrap(frames int64) (out int64, wrapped bool) {
	if u.Day <= 0 {
		u.Day = 86400
	}
	if !u.seen {
		u.prev, u.seen = frames, true
		return frames + int64(u.Rollover)*u.Day, false
	}
	if frames < u.prev && u.prev-frames > u.Day/2 {
		u.Rollover++
		wrapped = true
	}
	u.prev = frames
	return frames + int64(u.Rollover)*u.Day, wrapped
}

// TickUnwrapper expands wrapping raw clock ticks (e.g. 32-bit RTP
// timestamps) into int64 ticks using the same half-modulus rule.
type TickUnwrapper struct {
	Wrap uint64
	prev uint64
	seen bool
	High int64
}

// NewTickUnwrapper builds an unwrapper; wrap == 0 means 2^32.
func NewTickUnwrapper(wrap uint64) *TickUnwrapper {
	if wrap == 0 {
		wrap = 1 << 32
	}
	return &TickUnwrapper{Wrap: wrap}
}

// Expand resolves one raw value to unwrapped ticks.
func (u *TickUnwrapper) Expand(v uint64) int64 {
	if u.Wrap == 0 {
		u.Wrap = 1 << 32
	}
	if !u.seen {
		u.seen, u.prev = true, v
		return int64(v)
	}
	half := u.Wrap / 2
	switch {
	case v < u.prev && u.prev-v > half:
		u.High += int64(u.Wrap)
	case v > u.prev && v-u.prev > half:
		if u.High >= int64(u.Wrap) {
			u.High -= int64(u.Wrap)
		}
	}
	u.prev = v
	return u.High + int64(v)
}

// ParseClockTimestamp parses SRT/WebVTT clock stamps: HH:MM:SS,mmm or
// HH:MM:SS.mmm with optional fractional digits, and MM:SS.mmm.
func ParseClockTimestamp(s string) (nanos int64, err error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, media.New(media.StageTimebase, media.InputTC, media.CodeInvalidTimestamp, "empty timestamp")
	}
	sep := byte(0)
	if i := strings.IndexByte(s, ','); i >= 0 {
		sep = ','
	} else if i := strings.IndexByte(s, '.'); i >= 0 {
		sep = '.'
	}
	frac := ""
	whole := s
	if sep != 0 {
		idx := strings.IndexByte(s, sep)
		whole, frac = s[:idx], s[idx+1:]
		if frac == "" || len(frac) > 9 {
			return 0, media.New(media.StageTimebase, media.InputTC, media.CodeInvalidTimestamp, "bad fractional part in %q", orig)
		}
		for _, c := range frac {
			if c < '0' || c > '9' {
				return 0, media.New(media.StageTimebase, media.InputTC, media.CodeInvalidTimestamp, "bad fractional part in %q", orig)
			}
		}
	}
	parts := strings.Split(whole, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, media.New(media.StageTimebase, media.InputTC, media.CodeInvalidTimestamp, "malformed timestamp %q", orig)
	}
	var vals []int64
	for _, p := range parts {
		v, e := strconv.ParseInt(p, 10, 64)
		if e != nil || v < 0 {
			return 0, media.New(media.StageTimebase, media.InputTC, media.CodeInvalidTimestamp, "malformed timestamp %q", orig)
		}
		vals = append(vals, v)
	}
	var h, m, sec int64
	if len(vals) == 3 {
		h, m, sec = vals[0], vals[1], vals[2]
	} else {
		m, sec = vals[0], vals[1]
	}
	if m > 59 || sec > 59 {
		return 0, media.New(media.StageTimebase, media.InputTC, media.CodeInvalidTimestamp, "timestamp %q out of range", orig)
	}
	fracNanos := int64(0)
	if frac != "" {
		digits := frac
		for len(digits) < 9 {
			digits += "0"
		}
		fracNanos, _ = strconv.ParseInt(digits, 10, 64)
	}
	nanos = ((h*3600 + m*60 + sec) * 1_000_000_000) + fracNanos
	return nanos, nil
}

// FormatClockTimestamp renders nanoseconds as HH:MM:SS.mmm.
func FormatClockTimestamp(nanos int64) string {
	if nanos < 0 {
		nanos = 0
	}
	ms := (nanos + 500_000) / 1_000_000
	h := ms / 3_600_000
	ms %= 3_600_000
	m := ms / 60_000
	ms %= 60_000
	s := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
