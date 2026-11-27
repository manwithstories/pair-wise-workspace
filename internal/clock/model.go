// Package clock defines the unified nanosecond time base, multi-standard
// timecode arithmetic (24/25/30/29.97 DF/NDF) and the structured error model
// shared by every pipeline stage.
package clock

import (
	"fmt"
)

// Time units in the unified integer nanosecond time base.
const (
	Nanosecond  int64 = 1
	Microsecond       = 1_000 * Nanosecond
	Millisecond       = 1_000 * Microsecond
	Second            = 1_000 * Millisecond
	Minute            = 60 * Second
	Hour              = 60 * Minute

	// MaxRepairDelta bounds any single timestamp correction to 500 ms.
	MaxRepairDelta = 500 * Millisecond

	// MaxInputBytes is the hard 4 GiB input ceiling.
	MaxInputBytes int64 = 4 << 30
)

// Rat is an exact signed rational a/b used for frame-rate math. No float
// arithmetic is used anywhere in the time base.
type Rat struct {
	Num int64
	Den int64
}

// FloorDiv returns floor(a*Num/Den), never overflowing on realistic media
// durations (values stay well inside int64 for multi-hour assets).
func (r Rat) FloorDiv(a int64) int64 {
	q := a * r.Num / r.Den
	rem := a * r.Num % r.Den
	if rem != 0 && ((a < 0) != (r.Num < 0)) {
		q--
	}
	return q
}

// CeilDiv returns ceil(a*Num/Den), the exact inverse pairing of FloorDiv for
// non-negative frame-aligned timestamps.
func (r Rat) CeilDiv(a int64) int64 {
	q := a * r.Num / r.Den
	rem := a * r.Num % r.Den
	if rem != 0 && ((a >= 0) == (r.Num >= 0)) {
		q++
	}
	return q
}

// FrameRate describes a frame standard. DropFrame only carries meaning for
// the 29.97/30000:1001 family; Nominal is the display label.
type FrameRate struct {
	Num       int64
	Den       int64
	DropFrame bool
	Nominal   string
}

// FramesPer10Min is the SMPTE frame count inside a ten-minute block.
func (f FrameRate) FramesPer10Min() int64 {
	return f.Num * 600 / f.Den
}

// Standard frame rates supported by the parser.
var (
	Rate24     = FrameRate{Num: 24, Den: 1, Nominal: "24"}
	Rate25     = FrameRate{Num: 25, Den: 1, Nominal: "25"}
	Rate30     = FrameRate{Num: 30, Den: 1, Nominal: "30"}
	Rate2997   = FrameRate{Num: 30000, Den: 1001, Nominal: "29.97"}
	Rate2997DF = FrameRate{Num: 30000, Den: 1001, DropFrame: true, Nominal: "29.97DF"}
)

// RatesByName resolves a standard label (e.g. "29.97DF").
var RatesByName = map[string]FrameRate{
	"24":      Rate24,
	"25":      Rate25,
	"30":      Rate30,
	"29.97":   Rate2997,
	"29.97DF": Rate2997DF,
}

// Timecode is an H:M:S:F tuple plus the rate it belongs to.
type Timecode struct {
	Hour, Min, Sec, Frame int
	Rate                  FrameRate
}

// String renders the standard SMPTE form (';' before frame for drop frame).
func (t Timecode) String() string {
	sep := byte(':')
	if t.Rate.DropFrame {
		sep = ';'
	}
	return fmt.Sprintf("%02d:%02d:%02d%c%02d", t.Hour, t.Min, t.Sec, sep, t.Frame)
}

// dropFrames returns the frame numbers skipped at each dropped minute
// (zero for non-drop standards): round(rate/15) -> 2 for the 30000/1001 family.
func dropFrames(f FrameRate) int64 {
	if !f.DropFrame {
		return 0
	}
	return (f.Num + 15*f.Den/2) / (15 * f.Den)
}

// nominalFPS is the integer label rate (30 for 29.97) used in drop-frame
// label-space arithmetic.
func nominalFPS(f FrameRate) int64 {
	return (f.Num + f.Den/2) / f.Den
}

// frameNumber converts an H:M:S:F tuple to a real frame number. Drop-frame
// tuples live in nominal label space, so the skipped label numbers are
// subtracted; it returns -1 when the tuple lands on a dropped label.
func frameNumber(f FrameRate, h, m, s, fr int) int64 {
	base := int64(h)*3600 + int64(m)*60 + int64(s)
	if f.DropFrame {
		if s == 0 && m%10 != 0 && fr < int(dropFrames(f)) {
			return -1
		}
		d := dropFrames(f)
		totalMin := int64(h)*60 + int64(m)
		// SMPTE ST 12-1: label frames minus the labels dropped at the start
		// of every non-ten-minute minute.
		raw := base*nominalFPS(f) + int64(fr)
		raw -= d * (totalMin - totalMin/10)
		return raw
	}
	return base*(f.Num/f.Den) + int64(fr)
}

// ValidTimecode rejects out-of-range fields and illegal (dropped) frame nums.
func ValidTimecode(f FrameRate, h, m, s, fr int) bool {
	if h < 0 || h > 23 || m < 0 || m > 59 || s < 0 || s > 59 || fr < 0 {
		return false
	}
	maxFrame := int(f.Num/f.Den) - 1
	if f.DropFrame {
		maxFrame = int(nominalFPS(f)) - 1
	}
	if fr > maxFrame {
		return false
	}
	if f.DropFrame && s == 0 && m%10 != 0 && fr < int(dropFrames(f)) {
		return false
	}
	return true
}

// framesToNanos maps a real frame number to its exact presentation timestamp
// (floor division). NanosToFrame uses ceiling division, so frame -> nanos ->
// frame is an exact round trip on every standard.
func framesToNanos(f FrameRate, frames int64) int64 {
	return frames * f.Den * Second / f.Num
}

// TimecodeToNanos converts a validated tuple to the unified time base. It
// returns a CodeIllegalFrame error for dropped or out-of-range frame numbers.
func TimecodeToNanos(f FrameRate, h, m, s, fr int) (int64, error) {
	if !ValidTimecode(f, h, m, s, fr) {
		return 0, NewError(CodeIllegalFrame, "", Interval{},
			fmt.Sprintf("illegal frame %02d:%02d:%02d:%02d for %s", h, m, s, fr, f.Nominal))
	}
	n := frameNumber(f, h, m, s, fr)
	return framesToNanos(f, n), nil
}

// NanosToTimecode converts a nanosecond timestamp back to a tuple under f.
// Ceiling division inverts the floor division used by framesToNanos.
func NanosToTimecode(f FrameRate, ns int64) Timecode {
	frames := Rat{Num: f.Num, Den: f.Den * Second}.CeilDiv(ns)
	if f.DropFrame {
		return dropFrameTimecode(f, frames)
	}
	fps := f.Num / f.Den
	h := int(frames / (fps * 3600))
	r := frames % (fps * 3600)
	m := int(r / (fps * 60))
	r %= fps * 60
	s := int(r / fps)
	return Timecode{Hour: h, Min: m, Sec: s, Frame: int(r % fps), Rate: f}
}

// dropFrameTimecode inverts the SMPTE ST 12-1 drop-frame mapping by walking
// label space: each short minute (minute ordinal not divisible by ten) has
// dropFrames fewer labels, every other minute is a full nominal minute. The
// walk is bounded to ten iterations per ten-minute block and mirrors the
// forward frameNumber definition exactly, so the two are exact inverses.
func dropFrameTimecode(f FrameRate, frames int64) Timecode {
	nf := nominalFPS(f)
	d := dropFrames(f)
	minute := int64(0)
	remaining := frames
	for {
		minuteLen := nf * 60
		if minute%10 != 0 {
			minuteLen -= d
		}
		if remaining < minuteLen {
			break
		}
		remaining -= minuteLen
		minute++
	}
	if minute%10 != 0 {
		remaining += d // re-enter nominal label space; first d labels absent
	}
	sec, fr := remaining/nf, remaining%nf
	return Timecode{
		Hour:  int(minute / 60),
		Min:   int(minute % 60),
		Sec:   int(sec),
		Frame: int(fr),
		Rate:  f,
	}
}

// ParseTimecode accepts "HH:MM:SS:FF" (NDF) and "HH:MM:SS;FF" (DF) and binds
// the tuple to the supplied standard.
func ParseTimecode(f FrameRate, text string) (Timecode, error) {
	var h, m, s, fr int
	var sep byte
	if _, err := fmt.Sscanf(text, "%02d:%02d:%02d%c%02d", &h, &m, &s, &sep, &fr); err != nil {
		return Timecode{}, NewError(CodeIllegalFrame, "", Interval{},
			fmt.Sprintf("unparseable timecode %q: %v", text, err))
	}
	if sep != ':' && sep != ';' {
		return Timecode{}, NewError(CodeIllegalFrame, "", Interval{},
			fmt.Sprintf("bad frame separator %q in %q", sep, text))
	}
	if sep == ';' && !f.DropFrame {
		return Timecode{}, NewError(CodeIllegalFrame, "", Interval{},
			fmt.Sprintf("drop separator on non-drop standard in %q", text))
	}
	if !ValidTimecode(f, h, m, s, fr) {
		return Timecode{}, NewError(CodeIllegalFrame, "", Interval{},
			fmt.Sprintf("illegal frame %q for %s", text, f.Nominal))
	}
	return Timecode{Hour: h, Min: m, Sec: s, Frame: fr, Rate: f}, nil
}
