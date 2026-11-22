// Package clock centralizes frame-rate arithmetic, SMPTE drop-frame
// conversion and cross-midnight timecode wrap handling. No unconvertible
// timecode is ever silently normalized to zero: every invalid input returns
// an explicit error.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

// Rate describes a frame rate. Num/Den gives the exact rate in frames per
// second (for example 30000/1001); DropFrame selects SMPTE drop-frame
// numbering, which is only valid for rates close to 29.97 or 59.94 fps.
type Rate struct {
	Num       uint32
	Den       uint32
	DropFrame bool
}

// Predefined supported rates.
var (
	Rate24     = Rate{Num: 24, Den: 1}
	Rate25     = Rate{Num: 25, Den: 1}
	Rate30     = Rate{Num: 30, Den: 1}
	Rate2997DF = Rate{Num: 30000, Den: 1001, DropFrame: true}
	Rate2997   = Rate{Num: 30000, Den: 1001}
)

// Valid reports whether the rate is one of the four mandated frame rates.
func (r Rate) Valid() bool {
	switch r {
	case Rate24, Rate25, Rate30, Rate2997, Rate2997DF:
		return true
	default:
		return false
	}
}

// NominalFPS returns the integer frame count used by SMPTE timecode labels
// (24, 25 or 30).
func (r Rate) NominalFPS() int {
	switch {
	case r.Num == 24 && r.Den == 1:
		return 24
	case r.Num == 25 && r.Den == 1:
		return 25
	default: // 30 and 30000/1001 both label as 30
		return 30
	}
}

// Timecode is a decoded SMPTE HH:MM:SS:FF label plus an explicit day count
// so cross-midnight arithmetic never collapses onto a single 24h cycle.
type Timecode struct {
	Hour    int
	Minute  int
	Second  int
	Frame   int
	Day     int
	Drop    bool
}

// dropFrames is the number of dropped frame labels from 00:00:00;00 up to
// the given hour:minute boundary at nominal 30 fps drop-frame timecode.
// Every minute drops two labels except minutes 0, 10, 20, ... of each hour.
func dropFrames(hour, minute int) int {
	totalMinutes := hour*60 + minute
	exempt := totalMinutes / 10 // tenth minutes drop nothing
	return 2 * (totalMinutes - exempt)
}

// Parse decodes "HH:MM:SS:FF" (non-drop) or "HH:MM:SS;FF" (drop-frame)
// labels. Structural errors and out-of-range fields are reported, never
// coerced to zero.
func Parse(label string) (Timecode, error) {
	if len(label) != 11 {
		return Timecode{}, fmt.Errorf("timecode %q has wrong length, want HH:MM:SS:FF", label)
	}
	sep := label[8]
	if (sep != ':' && sep != ';') || label[2] != ':' || label[5] != ':' {
		return Timecode{}, fmt.Errorf("timecode %q has wrong separators", label)
	}
	v := make([]int, 4)
	pos := [4]int{0, 3, 6, 9}
	for i, p := range pos {
		if label[p] < '0' || label[p] > '9' || label[p+1] < '0' || label[p+1] > '9' {
			return Timecode{}, fmt.Errorf("timecode %q contains non-digit fields", label)
		}
		v[i] = int(label[p]-'0')*10 + int(label[p+1]-'0')
	}
	tc := Timecode{Hour: v[0], Minute: v[1], Second: v[2], Frame: v[3], Drop: sep == ';'}
	if tc.Minute > 59 || tc.Second > 59 || tc.Hour > 23 {
		return Timecode{}, fmt.Errorf("timecode %q has field out of range", label)
	}
	return tc, nil
}

// ToFrames converts a timecode label to an absolute frame index under rate r.
// Drop-frame labels may only be used with a drop-frame rate and vice versa.
func ToFrames(tc Timecode, r Rate) (int64, error) {
	if !r.Valid() {
		return 0, fmt.Errorf("unsupported frame rate %d/%d df=%v", r.Num, r.Den, r.DropFrame)
	}
	if tc.Drop != r.DropFrame {
		return 0, fmt.Errorf("timecode drop-frame flag %v does not match rate df=%v", tc.Drop, r.DropFrame)
	}
	fps := r.NominalFPS()
	if tc.Frame >= fps {
		return 0, fmt.Errorf("frame number %d exceeds fps %d", tc.Frame, fps)
	}
	// At non-tenth minutes frame label 1 is dropped (label 0 is valid).
	if r.DropFrame && tc.Second == 0 && tc.Frame == 1 && tc.Minute%10 != 0 {
		return 0, fmt.Errorf("drop-frame timecode %02d:%02d:%02d;%02d uses dropped frame label",
			tc.Hour, tc.Minute, tc.Second, tc.Frame)
	}
	frames := int64(tc.Hour)*3600*int64(fps) +
		int64(tc.Minute)*60*int64(fps) +
		int64(tc.Second)*int64(fps) + int64(tc.Frame)
	if r.DropFrame {
		frames -= int64(dropFrames(tc.Hour, tc.Minute))
	}
	frames += int64(tc.Day) * 24 * 3600 * int64(fps)
	return frames, nil
}

// FromFrames renders an absolute frame index as a timecode label under r.
func FromFrames(frames int64, r Rate) (Timecode, error) {
	if frames < 0 {
		return Timecode{}, errors.New("negative frame index cannot be rendered")
	}
	if !r.Valid() {
		return Timecode{}, fmt.Errorf("unsupported frame rate %d/%d", r.Num, r.Den)
	}
	fps := int64(r.NominalFPS())
	dayFrames := 24 * 3600 * fps
	day := int(frames / dayFrames)
	within := frames % dayFrames
	tc := Timecode{Day: day, Drop: r.DropFrame}
	if r.DropFrame {
		// One drop-frame hour holds 107892 real frames; split off the hour,
		// then invert the within-hour map by binary searching minutes 0..59.
		hourFrames := int64(107892)
		tc.Hour = int(within / hourFrames)
		rem := within % hourFrames
		lo, hi := 0, 60
		for lo < hi {
			mid := (lo + hi) / 2
			start := int64(mid)*1800 - int64(2*(mid-mid/10))
			if start > rem {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		minute := lo - 1
		dropped := int64(2 * (minute - minute/10))
		labels := rem - int64(minute)*1800 + dropped
		tc.Minute = minute
		tc.Second = int(labels / fps)
		tc.Frame = int(labels % fps)
		return tc, nil
	}
	tc.Hour = int(within / (3600 * fps))
	rem := within % (3600 * fps)
	tc.Minute = int(rem / (60 * fps))
	rem %= 60 * fps
	tc.Second = int(rem / fps)
	tc.Frame = int(rem % fps)
	return tc, nil
}

// ResolveWrap interprets a label relative to the previous absolute frame
// index: if the label is numerically smaller, it is treated as the next day
// rather than a jump backwards. A later label on the same day is an error.
func ResolveWrap(tc Timecode, r Rate, prevFrames int64) (int64, error) {
	frames, err := ToFrames(tc, r)
	if err != nil {
		return 0, err
	}
	dayFrames := int64(24 * 3600 * r.NominalFPS())
	base := frames % dayFrames
	if base < prevFrames {
		day := prevFrames/dayFrames + 1
		frames = day*dayFrames + base
	}
	if frames < prevFrames {
		return 0, fmt.Errorf("timecode went backwards: frame %d after %d", frames, prevFrames)
	}
	return frames, nil
}

// FramesToMillis converts an absolute frame index to milliseconds using the
// exact rate, so 29.97 fps does not drift.
func FramesToMillis(frames int64, r Rate) (int64, error) {
	if frames < 0 {
		return 0, errors.New("negative frame index")
	}
	if !r.Valid() {
		return 0, fmt.Errorf("unsupported frame rate %d/%d", r.Num, r.Den)
	}
	// millis = frames * 1000 * Den / Num
	num := new(big.Int).Mul(big.NewInt(frames*1000), big.NewInt(int64(r.Den)))
	q, rem := new(big.Int).QuoRem(num, big.NewInt(int64(r.Num)), new(big.Int))
	if rem.Sign() != 0 { // round to nearest millisecond
		twice := new(big.Int).Mul(rem, big.NewInt(2))
		if twice.Cmp(big.NewInt(int64(r.Num))) >= 0 {
			q.Add(q, big.NewInt(1))
		}
	}
	return q.Int64(), nil
}

// String renders the canonical HH:MM:SS:FF / HH:MM:SS;FF form.
func (tc Timecode) String() string {
	sep := ':'
	if tc.Drop {
		sep = ';'
	}
	return fmt.Sprintf("%02d:%02d:%02d%c%02d", tc.Hour, tc.Minute, tc.Second, sep, tc.Frame)
}
