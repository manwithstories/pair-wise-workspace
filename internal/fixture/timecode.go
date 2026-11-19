package fixture

import (
	"fmt"
	"math/big"
	"time"

	"pairflow/media/internal/clock"
)

// Rate is a broadcast frame rate expressed as an exact fraction.
type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Rates supported by the SMPTE timecode model.
var (
	Rate24       = Rate{24, 1}
	Rate25       = Rate{25, 1}
	Rate30       = Rate{30, 1}
	RateDrop2997 = Rate{30000, 1001}
)

// String renders "24", "25", "30" or "30000/1001".
func (r Rate) String() string {
	if r.Den == 1 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// FramesPerSecond returns the nominal integer rate used by SMPTE
// timecode counting.
func (r Rate) FramesPerSecond() int64 {
	switch {
	case r.Num == 30000 && r.Den == 1001:
		return 30
	case r.Num == 24000 && r.Den == 1001:
		return 24
	case r.Den == 1 && r.Num > 0:
		return r.Num
	}
	return (r.Num + r.Den/2) / r.Den
}

// DropFrame reports whether timecode uses the NTSC drop-frame rule.
func (r Rate) DropFrame() bool { return r.Num == 30000 && r.Den == 1001 }

// FrameDuration returns the exact duration of one frame.
func (r Rate) FrameDuration() clock.Rational {
	return clock.NewRational(r.Den, r.Num)
}

// FrameAt returns the exact presentation time of frame index n.
func (r Rate) FrameAt(n int64) clock.Rational {
	return clock.NewRational(n*r.Den, r.Num)
}

// Rate2398 is 24000/1001, the film-rate NTSC cousin of 24 fps. It uses
// integer SMPTE counting at nominal 24 fps and no drop-frame rule,
// which matches how 23.976 material is labelled in practice.
var Rate2398 = Rate{24000, 1001}

// ParseRate accepts the supported rates, spelled either as an integer,
// a common decimal alias or an exact fraction.
func ParseRate(s string) (Rate, error) {
	for _, r := range []Rate{Rate24, Rate25, Rate30, RateDrop2997, Rate2398} {
		if s == r.String() {
			return r, nil
		}
	}
	switch s {
	case "23.976", "23.98":
		return Rate2398, nil
	case "29.97":
		return RateDrop2997, nil
	}
	return Rate{}, fmt.Errorf("unsupported frame rate %q (want 24, 25, 30, 30000/1001 or 24000/1001)", s)
}

// Timecode is a SMPTE non-drop or drop-frame label.
type Timecode struct {
	Hours   int  `json:"hours"`
	Minutes int  `json:"minutes"`
	Seconds int  `json:"seconds"`
	Frames  int  `json:"frames"`
	Drop    bool `json:"drop_frame"`
}

// String renders HH:MM:SS:FF (non-drop) or HH:MM:SS;FF (drop).
func (tc Timecode) String() string {
	sep := ":"
	if tc.Drop {
		sep = ";"
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%02d", tc.Hours, tc.Minutes, tc.Seconds, sep, tc.Frames)
}

// FrameNumber converts a timecode label to an absolute frame count.
// Drop-frame labels are expanded with the 10-minute compensation rule.
func (tc Timecode) FrameNumber(rate Rate) (int64, error) {
	if tc.Hours < 0 || tc.Minutes < 0 || tc.Seconds < 0 || tc.Frames < 0 {
		return 0, fmt.Errorf("timecode field out of range: %s", tc)
	}
	if tc.Minutes > 59 || tc.Seconds > 59 {
		return 0, fmt.Errorf("timecode field out of range: %s", tc)
	}
	fps := rate.FramesPerSecond()
	if int64(tc.Frames) >= fps {
		return 0, fmt.Errorf("frame field %d >= %d fps", tc.Frames, fps)
	}
	total := int64(tc.Hours)*3600*fps + int64(tc.Minutes)*60*fps + int64(tc.Seconds)*fps + int64(tc.Frames)
	if tc.Drop {
		if !rate.DropFrame() {
			return 0, fmt.Errorf("drop-frame timecode requires 30000/1001, got %s", rate)
		}
		minutes := int64(tc.Hours)*60 + int64(tc.Minutes)
		total -= 2 * (minutes - minutes/10)
	} else if rate.DropFrame() {
		return 0, fmt.Errorf("drop-frame rate requires drop-frame timecode")
	}
	return total, nil
}

// TimecodeAt converts an absolute frame count to a timecode label. The
// drop-frame expansion follows SMPTE 12M: every minute except each
// tenth skips labels 00 and 01.
func TimecodeAt(frame int64, rate Rate) Timecode {
	if frame < 0 {
		frame = 0
	}
	fps := rate.FramesPerSecond()
	adjusted := frame
	if rate.DropFrame() {
		const per10 = int64(17982) // 10 minutes at 29.97, minus 18 labels
		d := frame / per10
		m := frame % per10
		if m < 1800 {
			adjusted = frame + 18*d
		} else {
			adjusted = frame + 18*(d+1) + 2*((m-1800)/1798)
		}
	}
	tc := Timecode{Drop: rate.DropFrame()}
	tc.Frames = int(adjusted % fps)
	secs := adjusted / fps
	tc.Seconds = int(secs % 60)
	tc.Minutes = int((secs / 60) % 60)
	tc.Hours = int(secs / 3600)
	return tc
}

// TimecodeFromSeconds converts an exact presentation time to its frame
// index and label, optionally wrapping at the 24-hour day boundary.
func TimecodeFromSeconds(t clock.Rational, rate Rate, wrapDay bool) (int64, Timecode, error) {
	if t.Num < 0 {
		return 0, Timecode{}, fmt.Errorf("negative presentation time %s", t)
	}
	frame := rateFrameIndex(t, rate)
	if wrapDay {
		framesPerDay := int64(24*3600) * rate.FramesPerSecond()
		if framesPerDay > 0 && frame >= framesPerDay {
			frame %= framesPerDay
		}
	}
	return frame, TimecodeAt(frame, rate), nil
}

// rateFrameIndex returns the exact frame index for t at rate, floored.
func rateFrameIndex(t clock.Rational, rate Rate) int64 {
	num := new(big.Int).Mul(big.NewInt(t.Num), big.NewInt(rate.Num))
	den := new(big.Int).Mul(big.NewInt(t.Den), big.NewInt(rate.Den))
	num.Quo(num, den)
	if !num.IsInt64() {
		return 0
	}
	return num.Int64()
}

// ParseTimecode validates a free-form timecode string.
func ParseTimecode(s string, rate Rate) (Timecode, int64, error) {
	var h, m, sec, f int
	drop := false
	n, err := fmt.Sscanf(s, "%02d:%02d:%02d:%02d", &h, &m, &sec, &f)
	if err != nil || n != 4 {
		n, err = fmt.Sscanf(s, "%02d:%02d:%02d;%02d", &h, &m, &sec, &f)
		if err != nil || n != 4 {
			return Timecode{}, 0, fmt.Errorf("bad timecode %q", s)
		}
		drop = true
	}
	tc := Timecode{Hours: h, Minutes: m, Seconds: sec, Frames: f, Drop: drop}
	frame, err := tc.FrameNumber(rate)
	if err != nil {
		return Timecode{}, 0, err
	}
	return tc, frame, nil
}

// StartOfEpoch is the reference wall-clock instant used by fixtures.
var StartOfEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
