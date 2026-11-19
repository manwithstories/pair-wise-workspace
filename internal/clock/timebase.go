// Package clock converts between frame numbers, seconds and SMPTE timecodes
// for fractional frame rates (24, 25, 30, 30000/1001), including drop-frame
// and day-wrap handling.
package clock

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Rate is a fractional frame rate: frames per second = Num/Den.
type Rate struct {
	Num int64
	Den int64
}

var (
	Rate24   = Rate{24, 1}
	Rate25   = Rate{25, 1}
	Rate30   = Rate{30, 1}
	Rate2997 = Rate{30000, 1001}
)

// ParseRate accepts "24", "25", "30", "30000/1001" (or "29.97").
func ParseRate(s string) (Rate, error) {
	switch strings.TrimSpace(s) {
	case "24", "24/1":
		return Rate24, nil
	case "25", "25/1":
		return Rate25, nil
	case "30", "30/1":
		return Rate30, nil
	case "30000/1001", "29.97":
		return Rate2997, nil
	}
	return Rate{}, fmt.Errorf("unsupported frame rate %q (want 24, 25, 30 or 30000/1001)", s)
}

func (r Rate) String() string {
	if r.Den == 1 {
		return strconv.FormatInt(r.Num, 10)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// Nominal returns the integer frame-number base (e.g. 30 for 30000/1001).
func (r Rate) Nominal() int64 { return (r.Num + r.Den/2) / r.Den }

// DropSupported reports whether SMPTE drop-frame is defined for this rate.
func (r Rate) DropSupported() bool { return r.Num == 30000 && r.Den == 1001 }

// dropPerMinute is the number of frame labels dropped each minute (except
// every tenth minute) in drop-frame mode.
func (r Rate) dropPerMinute() int64 {
	if r.DropSupported() {
		return 2
	}
	return 0
}

// FramesPerDay returns the number of frame labels in one 24-hour day,
// accounting for dropped labels in drop-frame mode.
func (r Rate) FramesPerDay(drop bool) int64 {
	day := r.Nominal() * 86400
	if drop && r.DropSupported() {
		day -= r.dropPerMinute() * (1440 - 144) // 2 dropped labels per minute, except x0 minutes
	}
	return day
}

// FramesToSeconds converts an absolute frame count to seconds.
func (r Rate) FramesToSeconds(frames int64) float64 {
	return float64(frames) * float64(r.Den) / float64(r.Num)
}

// SecondsToFrames converts seconds to the nearest frame number.
func (r Rate) SecondsToFrames(sec float64) int64 {
	return int64(math.Round(sec * float64(r.Num) / float64(r.Den)))
}

// DayWraps reports whether the clock wrapped past midnight between two
// frames-in-day values (a backward jump of more than half a day).
func (r Rate) DayWraps(prevInDay, curInDay int64, drop bool) bool {
	return curInDay < prevInDay-r.FramesPerDay(drop)/2
}

// Timecode is a SMPTE timecode with an absolute day counter for wrap-around.
type Timecode struct {
	Day  int
	HH   int
	MM   int
	SS   int
	FF   int
	Drop bool
}

var tcRe = regexp.MustCompile(`^(\d{1,2}):(\d{2}):(\d{2})([:;])(\d{1,2})$`)

// ParseTimecode parses "HH:MM:SS:FF" (non-drop) or "HH:MM:SS;FF" (drop).
func ParseTimecode(s string) (Timecode, error) {
	m := tcRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Timecode{}, fmt.Errorf("malformed timecode %q", s)
	}
	hh, _ := strconv.Atoi(m[1])
	mm, _ := strconv.Atoi(m[2])
	ss, _ := strconv.Atoi(m[3])
	ff, _ := strconv.Atoi(m[5])
	tc := Timecode{HH: hh, MM: mm, SS: ss, FF: ff, Drop: m[4] == ";"}
	if hh > 23 || mm > 59 || ss > 59 {
		return tc, fmt.Errorf("timecode %q out of range", s)
	}
	return tc, nil
}

// ToFrames converts the timecode to an absolute frame number for the rate,
// including the day offset. Unconvertible values return an error so the
// caller can keep the original text as a located error record.
func (tc Timecode) ToFrames(r Rate) (int64, error) {
	nom := r.Nominal()
	if int64(tc.FF) >= nom {
		return 0, fmt.Errorf("frame %d exceeds nominal rate %d", tc.FF, nom)
	}
	if tc.Drop && !r.DropSupported() {
		return 0, fmt.Errorf("drop-frame not supported at %s", r)
	}
	totalMin := int64(tc.HH)*60 + int64(tc.MM)
	base := (int64(tc.HH)*3600+int64(tc.MM)*60+int64(tc.SS))*nom + int64(tc.FF)
	if !tc.Drop {
		return base + int64(tc.Day)*r.FramesPerDay(false), nil
	}
	d := r.dropPerMinute()
	if tc.SS == 0 && tc.MM%10 != 0 && int64(tc.FF) < d {
		return 0, fmt.Errorf("dropped frame label %02d:%02d:%02d;%02d", tc.HH, tc.MM, tc.SS, tc.FF)
	}
	return base - d*(totalMin-totalMin/10) + int64(tc.Day)*r.FramesPerDay(true), nil
}

// FormatTimecode renders an absolute frame number as a SMPTE timecode.
func FormatTimecode(frames int64, r Rate, drop bool) (string, error) {
	if frames < 0 {
		return "", fmt.Errorf("negative frame number %d", frames)
	}
	if drop && !r.DropSupported() {
		return "", fmt.Errorf("drop-frame not supported at %s", r)
	}
	nom := r.Nominal()
	frames %= r.FramesPerDay(drop)
	adj := frames
	sep := byte(':')
	if drop {
		sep = ';'
		d := r.dropPerMinute()
		fp10m := nom*600 - d*9 // labels per 10-minute cycle
		fpm := nom*60 - d      // labels per dropped minute
		tens := frames / fp10m
		m := frames % fp10m
		adj = frames + d*9*tens
		if m >= d {
			adj += d * ((m - d) / fpm)
		}
	}
	hh := adj / (nom * 3600)
	adj %= nom * 3600
	mm := adj / (nom * 60)
	adj %= nom * 60
	return fmt.Sprintf("%02d:%02d:%02d%c%02d", hh, mm, adj/nom, sep, adj%nom), nil
}
