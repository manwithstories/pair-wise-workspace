// Package clock converts SMPTE timecode to and from the unified time base.
// Supported rates: 24, 25, 30 and 30000/1001 fps, drop-frame and
// non-drop-frame. Day wrap (crossing 24:00:00:00) is handled explicitly and
// illegal frame numbers are rejected.
package clock

import (
	"fmt"
	"strconv"
	"strings"

	"presspack/internal/core"
)

// FPS is a rational frame rate.
type FPS struct {
	Num int64 // frames
	Den int64 // seconds
}

// Supported returns the four supported frame rates keyed by their canonical
// string form.
func Supported() map[string]FPS {
	return map[string]FPS{
		"24":         {24, 1},
		"25":         {25, 1},
		"30":         {30, 1},
		"30000/1001": {30000, 1001},
	}
}

// ParseFPS accepts "24", "25", "30", "30000/1001" (and the alias "29.97").
func ParseFPS(s string) (FPS, error) {
	if s == "29.97" {
		s = "30000/1001"
	}
	if f, ok := Supported()[s]; ok {
		return f, nil
	}
	return FPS{}, fmt.Errorf("unsupported fps %q: want 24, 25, 30 or 30000/1001", s)
}

// Nominal is the integer frame count used for timecode labelling
// (30 for 30000/1001).
func (f FPS) Nominal() int64 {
	return (f.Num + f.Den - 1) / f.Den
}

// FrameDuration is the exact duration of one frame in global ticks.
func (f FPS) FrameDuration() core.Time {
	return core.FromUnits(f.Den, core.Rat(1, f.Num))
}

// DropFrameAllowed reports whether drop-frame labelling is meaningful at
// this rate: only 30000/1001.
func (f FPS) DropFrameAllowed() bool { return f.Num == 30000 && f.Den == 1001 }

// Timecode is a parsed SMPTE label.
type Timecode struct {
	Hour, Min, Sec, Frame int
	Drop                  bool // separator ';' before frame
}

// Parse reads "HH:MM:SS:FF" (non-drop) or "HH:MM:SS;FF" (drop-frame).
func Parse(s string) (Timecode, error) {
	s = strings.TrimSpace(s)
	drop := strings.Contains(s, ";")
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == ';' })
	if len(parts) != 4 {
		return Timecode{}, fmt.Errorf("timecode %q: want HH:MM:SS:FF", s)
	}
	var v [4]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Timecode{}, fmt.Errorf("timecode %q: bad field %q", s, p)
		}
		v[i] = n
	}
	return Timecode{Hour: v[0], Min: v[1], Sec: v[2], Frame: v[3], Drop: drop}, nil
}

// Validate checks the label against a rate: field ranges, frame number
// below the nominal rate, and drop-frame skip rules (frames 00 and 01 are
// skipped at every minute except multiples of ten).
func (tc Timecode) Validate(fps FPS) error {
	if tc.Hour > 23 || tc.Min > 59 || tc.Sec > 59 {
		return fmt.Errorf("timecode %v out of range", tc)
	}
	if tc.Frame < 0 || int64(tc.Frame) >= fps.Nominal() {
		return fmt.Errorf("illegal frame number %d at nominal %d", tc.Frame, fps.Nominal())
	}
	if tc.Drop {
		if !fps.DropFrameAllowed() {
			return fmt.Errorf("drop-frame only valid at 30000/1001")
		}
		if tc.Sec == 0 && tc.Min%10 != 0 && tc.Frame < 2 {
			return fmt.Errorf("dropped label %02d:%02d:00;%02d does not exist",
				tc.Hour, tc.Min, tc.Frame)
		}
	}
	return nil
}

// ToFrameNumber converts a validated label to an absolute frame number
// within its 24-hour day, applying drop-frame compensation.
func (tc Timecode) ToFrameNumber(fps FPS) int64 {
	nom := fps.Nominal()
	total := int64(tc.Hour*3600+tc.Min*60+tc.Sec)*nom + int64(tc.Frame)
	if tc.Drop {
		totalMinutes := int64(tc.Hour*60 + tc.Min)
		total -= 2 * (totalMinutes - totalMinutes/10)
	}
	return total
}

// FramesPerDay returns the number of real frames in 24 hours at this rate.
func FramesPerDay(fps FPS, drop bool) int64 {
	total := int64(24*3600) * fps.Nominal()
	if drop {
		totalMinutes := int64(24 * 60)
		total -= 2 * (totalMinutes - totalMinutes/10)
	}
	return total
}

// ToTime converts a label to global ticks. day is the wrap count supplied by
// the caller (see ResolveSequence); the label itself never exceeds 23:59:59.
func ToTime(tc Timecode, fps FPS, day int64) (core.Time, error) {
	if err := tc.Validate(fps); err != nil {
		return core.Time{}, err
	}
	fn := tc.ToFrameNumber(fps) + day*FramesPerDay(fps, tc.Drop)
	return core.FromUnits(fn*fps.Den, core.Rat(1, fps.Num)), nil
}

// ResolveSequence converts an ordered list of labels to global times,
// detecting 24-hour wrap: whenever a label is smaller than its predecessor
// the day counter advances and a recoverable FaultTimecodeWrap is recorded.
// Invalid labels produce non-recoverable FaultInvalidTimecode entries and a
// nil time for that slot.
func ResolveSequence(labels []string, fps FPS) ([]core.Time, core.Faults) {
	times := make([]core.Time, len(labels))
	var faults core.Faults
	var day, prevFN int64
	started := false
	for i, s := range labels {
		tc, err := Parse(s)
		if err != nil {
			faults = append(faults, core.NewFault(core.FaultInvalidTimecode, int64(i), false, err.Error()))
			continue
		}
		if err := tc.Validate(fps); err != nil {
			faults = append(faults, core.NewFault(core.FaultInvalidTimecode, int64(i), false, err.Error()))
			continue
		}
		fn := tc.ToFrameNumber(fps)
		if started && fn < prevFN {
			day++
			faults = append(faults, core.NewFault(core.FaultTimecodeWrap, int64(i), true,
				fmt.Sprintf("label %s wraps past 24h (day %d)", s, day)))
		}
		t, err := ToTime(tc, fps, day)
		if err != nil {
			faults = append(faults, core.NewFault(core.FaultInvalidTimecode, int64(i), false, err.Error()))
			continue
		}
		times[i] = t
		prevFN, started = fn, true
	}
	return times, faults
}
