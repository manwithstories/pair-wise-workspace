package media

import (
	"fmt"
	"strconv"
	"strings"
)

// Timecode is a parsed SMPTE timecode on an exact nanosecond base.
//
// The textual form is HH:MM:SS:FF (non-drop) or HH:MM:SS;FF (drop-frame),
// optionally prefixed by '-' for a negative offset. Hours are allowed to run
// past 23 to represent multi-day material; DayOffset records whole-day wraps
// that were explicitly present in the source ("D+1/01:00:00:00" style is
// accepted by ParseTimecodeWithDay).
type Timecode struct {
	// Hours/Minutes/Seconds/Frames are the literal digit fields as written.
	Hours, Minutes, Seconds, Frames int
	// DropFrame records the ';' separator (SMPTE drop-frame counting).
	DropFrame bool
	// Negative marks a leading '-'.
	Negative bool
	// DayOffset holds whole 24h wraps carried by a "D+N/" prefix.
	DayOffset int
	// Rate is the frame rate this timecode was interpreted at.
	Rate FrameRate
	// Ns is the exact nanosecond value, negative when Negative is set.
	Ns int64
}

// TimecodeDefect describes a recoverable irregularity found while parsing.
type TimecodeDefect struct {
	Class   Class
	Message string
}

// ParseTimecode parses "HH:MM:SS:FF" / "HH:MM:SS;FF" at the given rate.
// It rejects out-of-range fields rather than silently wrapping them.
func ParseTimecode(s string, rate FrameRate) (Timecode, []TimecodeDefect, error) {
	return ParseTimecodeWithDay(s, rate, 0)
}

// ParseTimecodeWithDay additionally accepts a "D+N/" prefix meaning N extra
// 24-hour days, which is how a long recording crosses midnight without
// resetting the timecode.
func ParseTimecodeWithDay(s string, rate FrameRate, dayHint int) (Timecode, []TimecodeDefect, error) {
	orig := strings.TrimSpace(s)
	var defects []TimecodeDefect
	neg := false
	if strings.HasPrefix(orig, "-") {
		neg = true
		orig = orig[1:]
	}
	if orig == "" {
		return Timecode{}, nil, fmt.Errorf("%w: empty timecode", ErrBadTimecode)
	}

	day := dayHint
	if strings.HasPrefix(orig, "D+") {
		rest := orig[2:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return Timecode{}, nil, fmt.Errorf("%w: malformed day prefix %q", ErrBadTimecode, s)
		}
		n, err := strconv.Atoi(rest[:slash])
		if err != nil || n < 0 || n > 9999 {
			return Timecode{}, nil, fmt.Errorf("%w: malformed day offset in %q", ErrBadTimecode, s)
		}
		day = n
		orig = rest[slash+1:]
	}

	// Find the two separators. Time fields use ':', frame field may use ':'
	// (NDF) or ';' (DF).
	parts := strings.FieldsFunc(orig, func(r rune) bool { return r == ':' || r == ';' })
	if len(parts) != 4 {
		return Timecode{}, nil, fmt.Errorf("%w: %q is not HH:MM:SS:FF", ErrBadTimecode, s)
	}
	// Determine which separator precedes the frame field.
	last := orig[len(orig)-3:]
	df := strings.ContainsRune(last, ';')

	vals := make([]int, 4)
	for i, p := range parts {
		if p == "" || len(p) > 4 {
			return Timecode{}, nil, fmt.Errorf("%w: field %q out of range in %q", ErrBadTimecode, p, s)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Timecode{}, nil, fmt.Errorf("%w: field %q is not an integer in %q", ErrBadTimecode, p, s)
		}
		if n < 0 {
			return Timecode{}, nil, fmt.Errorf("%w: negative field in %q", ErrBadTimecode, s)
		}
		vals[i] = n
	}
	h, m, sec, fr := vals[0], vals[1], vals[2], vals[3]

	if df && !rate.SupportsDropFrame() {
		return Timecode{}, nil, fmt.Errorf("%w: drop-frame separator ';' used with rate %s",
			ErrBadTimecode, rate.Label())
	}
	if rate.DropFrame && !df {
		defects = append(defects, TimecodeDefect{
			Class:   ClassTimecodeBadDropFrame,
			Message: "drop-frame rate written with ':' separator; treating as drop-frame",
		})
		df = true
	}
	if m > 59 || sec > 59 {
		return Timecode{}, nil, fmt.Errorf("%w: minute/second out of range in %q", ErrBadTimecode, s)
	}
	nominal := rate.NominalFPS()
	if fr >= int(nominal) {
		return Timecode{}, nil, fmt.Errorf("%w: frame field %d >= %d fps in %q", ErrBadTimecode, fr, nominal, s)
	}
	if df {
		// In DF counting, frame numbers 00 and 01 (or 00..03) are skipped at
		// the start of every minute except minutes divisible by 10. A label
		// that names a skipped frame is illegal.
		if m%10 != 0 && sec == 0 && int64(fr) < rate.dropCountPerMinute() {
			return Timecode{}, nil, fmt.Errorf("%w: %q names a skipped drop-frame label",
				ErrBadTimecode, s)
		}
	}
	// Hours beyond 23 are allowed but must be reported as extended.
	if h > 23 && day == dayHint {
		defects = append(defects, TimecodeDefect{
			Class:   ClassTimecodeOutOfRange,
			Message: fmt.Sprintf("hour field %d exceeds 23; treated as extended timecode", h),
		})
	}

	tc := Timecode{
		Hours: h, Minutes: m, Seconds: sec, Frames: fr,
		DropFrame: df, Negative: neg, DayOffset: day, Rate: rate,
	}
	ns, err := tc.toNanos()
	if err != nil {
		return Timecode{}, defects, err
	}
	tc.Ns = ns
	if neg {
		tc.Ns = -ns
	}
	return tc, defects, nil
}

// Sentinel errors exposed for errors.Is by callers.
var (
	ErrBadTimecode = &timecodeError{msg: "bad timecode"}
)

type timecodeError struct{ msg string }

func (e *timecodeError) Error() string { return e.msg }

// totalFrames converts the label to an absolute frame count.
func (tc Timecode) totalFrames() (int64, error) {
	nominal := tc.Rate.NominalFPS()
	if nominal <= 0 {
		return 0, fmt.Errorf("%w: zero nominal rate", ErrBadTimecode)
	}
	frames := int64(tc.Hours)*3600*nominal +
		int64(tc.Minutes)*60*nominal +
		int64(tc.Seconds)*nominal +
		int64(tc.Frames)
	if tc.DropFrame {
		totalMinutes := int64(tc.Hours)*60 + int64(tc.Minutes)
		// Every minute not divisible by 10 drops dropCount frame numbers.
		frames -= tc.Rate.dropCountPerMinute() * (totalMinutes - totalMinutes/10)
	}
	if frames < 0 {
		return 0, fmt.Errorf("%w: negative frame count", ErrBadTimecode)
	}
	// Add whole-day offsets.
	frames += int64(tc.DayOffset) * 24 * 3600 * nominal
	return frames, nil
}

func (tc Timecode) toNanos() (int64, error) {
	frames, err := tc.totalFrames()
	if err != nil {
		return 0, err
	}
	return tc.Rate.FrameToNanos(frames)
}

// Nanos returns the signed nanosecond value (negative offsets preserved).
func (tc Timecode) Nanos() int64 { return tc.Ns }

// String renders the timecode in canonical form, including the day prefix.
func (tc Timecode) String() string {
	sep := ":"
	if tc.DropFrame {
		sep = ";"
	}
	body := fmt.Sprintf("%02d:%02d:%02d%s%02d", tc.Hours, tc.Minutes, tc.Seconds, sep, tc.Frames)
	if tc.DayOffset != 0 {
		body = fmt.Sprintf("D+%d/%s", tc.DayOffset, body)
	}
	if tc.Negative {
		return "-" + body
	}
	return body
}

// TimecodeFromFrames builds a canonical timecode label from an absolute frame
// count. It is the inverse of totalFrames for non-negative inputs and is used
// to render late/recovered events deterministically.
func TimecodeFromFrames(frame int64, rate FrameRate) (Timecode, error) {
	if frame < 0 {
		return Timecode{}, fmt.Errorf("%w: negative frame count %d", ErrBadTimecode, frame)
	}
	nominal := rate.NominalFPS()
	drop := rate.DropFrame
	frames := frame
	if drop {
		// The reference drop-frame inverse (SMPTE 12M / A. Duncan):
		//   d = frame / framesPer10Minutes
		//   m = frame % framesPer10Minutes
		//   if m > dropCount: frame += dropCount*9*d + dropCount*((m-dropCount)/framesPerMinute)
		//   else:             frame += dropCount*9*d
		// where framesPer10Minutes is the real frame count of ten minutes.
		dropCount := rate.dropCountPerMinute()
		framesPerMinute := nominal*60 - dropCount
		framesPer10Minutes := framesPerMinute*10 + dropCount
		d := frames / framesPer10Minutes
		m := frames % framesPer10Minutes
		frames += dropCount * 9 * d
		if m > dropCount {
			frames += dropCount * ((m - dropCount) / framesPerMinute)
		}
	}
	totalSeconds := frames / nominal
	fr := frames % nominal
	sec := totalSeconds % 60
	totalMinutes := totalSeconds / 60
	min := totalMinutes % 60
	hour := totalMinutes / 60
	if hour > 23 {
		// Keep extended hours but record whole days.
		days := hour / 24
		hour = hour % 24
		return Timecode{
			Hours: int(hour), Minutes: int(min), Seconds: int(sec), Frames: int(fr),
			DropFrame: drop, DayOffset: int(days), Rate: rate,
		}, nil
	}
	return Timecode{
		Hours: int(hour), Minutes: int(min), Seconds: int(sec), Frames: int(fr),
		DropFrame: drop, Rate: rate,
	}, nil
}
