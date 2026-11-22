// Package clock unifies fractional frame rates, cross-day timecode wrap,
// and RTP timestamp conversion onto a single shared media timebase.
package clock

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Error is the unified error type shared by the timebase converter and the
// subtitle parsers. Line is the 1-based source line, 0 when not applicable.
type Error struct {
	Op   string
	Line int
	Msg  string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s: line %d: %s", e.Op, e.Line, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Msg)
}

// Rational is a fraction Num/Den, e.g. 30000/1001 fps.
type Rational struct {
	Num int64
	Den int64
}

// ParseRational parses "30000/1001" or "30".
func ParseRational(s string) (Rational, error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) == 0 || len(parts) > 2 {
		return Rational{}, &Error{Op: "clock.ParseRational", Msg: "invalid rational: " + s}
	}
	num, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || num <= 0 {
		return Rational{}, &Error{Op: "clock.ParseRational", Msg: "invalid numerator: " + s}
	}
	den := int64(1)
	if len(parts) == 2 {
		den, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || den <= 0 {
			return Rational{}, &Error{Op: "clock.ParseRational", Msg: "invalid denominator: " + s}
		}
	}
	return Rational{Num: num, Den: den}, nil
}

func (r Rational) Float64() float64 { return float64(r.Num) / float64(r.Den) }

// Supported reports whether s names a supported frame rate:
// 24, 25, 30 or 30000/1001.
func Supported(s string) bool {
	switch strings.TrimSpace(s) {
	case "24", "25", "30", "30000/1001":
		return true
	}
	return false
}

// RTPClockRate is the MPEG-TS/RTP media clock rate in Hz.
const RTPClockRate = 90000

// Timebase converts between frames, durations, timecodes and RTP timestamps.
type Timebase struct {
	FPS Rational
}

// LabelRate is the integer timecode label rate (ceil of the fps), i.e. the
// number of frame labels per second. Timecode labels are a pure numbering
// scheme; convert to media frames with TimecodeToMedia.
func (tb Timebase) LabelRate() int64 {
	return (tb.FPS.Num + tb.FPS.Den - 1) / tb.FPS.Den
}

// FramesPerDay is the number of timecode labels in 24h, used for
// cross-day wrap. It is always an integer.
func (tb Timebase) FramesPerDay() int64 {
	return 24 * 3600 * tb.LabelRate()
}

// DurationToFrames converts a duration to a frame count (rounded).
func (tb Timebase) DurationToFrames(d time.Duration) int64 {
	return (int64(d)*tb.FPS.Num + tb.FPS.Den*int64(time.Second)/2) /
		(tb.FPS.Den * int64(time.Second))
}

// FramesToDuration converts a frame count to a duration.
func (tb Timebase) FramesToDuration(f int64) time.Duration {
	return time.Duration(f * tb.FPS.Den * int64(time.Second) / tb.FPS.Num)
}

// MillisToFrames converts milliseconds to a frame count (rounded).
func (tb Timebase) MillisToFrames(ms int64) int64 {
	return (ms*tb.FPS.Num + tb.FPS.Den*500) / (tb.FPS.Den * 1000)
}

// ParseTimecode parses "HH:MM:SS:FF" into frames since midnight.
func (tb Timebase) ParseTimecode(s string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 4 {
		return 0, &Error{Op: "clock.ParseTimecode", Msg: "want HH:MM:SS:FF, got " + s}
	}
	v := make([]int64, 4)
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return 0, &Error{Op: "clock.ParseTimecode", Msg: "bad field in " + s}
		}
		v[i] = n
	}
	if v[1] > 59 || v[2] > 59 || v[3] >= tb.LabelRate() {
		return 0, &Error{Op: "clock.ParseTimecode", Msg: "field out of range in " + s}
	}
	sec := v[0]*3600 + v[1]*60 + v[2]
	return sec*tb.LabelRate() + v[3], nil
}

// FormatTimecode renders frames since midnight as "HH:MM:SS:FF".
func (tb Timebase) FormatTimecode(f int64) string {
	f = tb.WrapDay(f)
	rate := tb.LabelRate()
	sec := f / rate
	return fmt.Sprintf("%02d:%02d:%02d:%02d", sec/3600, (sec/60)%60, sec%60, f%rate)
}

// TimecodeToMedia converts a timecode label count to media frames.
func (tb Timebase) TimecodeToMedia(tc int64) int64 {
	return tc * tb.FPS.Num / (tb.FPS.Den * tb.LabelRate())
}

// WrapDay wraps a frame count into [0, FramesPerDay).
func (tb Timebase) WrapDay(f int64) int64 {
	day := tb.FramesPerDay()
	return ((f % day) + day) % day
}

// UnwrapDay adjusts cur by whole days so it stays within half a day of
// prev, resolving cross-midnight wraparound in monotonic order.
func (tb Timebase) UnwrapDay(prev, cur int64) int64 {
	day := tb.FramesPerDay()
	for cur+day/2 < prev {
		cur += day
	}
	for cur-day/2 > prev {
		cur -= day
	}
	return cur
}

// RTPToFrame converts an RTP 90kHz timestamp to a frame number, given a
// known (baseRTP, baseFrame) anchor. uint32 subtraction handles RTP wrap.
func (tb Timebase) RTPToFrame(rtp, baseRTP uint32, baseFrame int64) int64 {
	delta := int64(int32(rtp - baseRTP))
	return baseFrame + delta*tb.FPS.Num/(tb.FPS.Den*RTPClockRate)
}

// FrameToRTP converts a frame number back to an RTP 90kHz timestamp.
func (tb Timebase) FrameToRTP(frame, baseFrame int64, baseRTP uint32) uint32 {
	delta := (frame - baseFrame) * tb.FPS.Den * RTPClockRate / tb.FPS.Num
	return baseRTP + uint32(delta)
}
