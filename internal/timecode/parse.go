// Package timecode parses camera timecodes onto the unified media clock.
// Supported rates: 24, 25, 30, 30000/1001 (29.97) and 29.97 drop-frame.
// Midnight wrap is resolved against a reference media time; ambiguous
// input is rejected rather than guessed.
package timecode

import (
	"fmt"
	"regexp"
	"strconv"

	"deliver/internal/model"
)

// FPS is a rational frame rate.
type FPS struct{ Num, Den int }

var (
	FPS24   = FPS{24, 1}
	FPS25   = FPS{25, 1}
	FPS30   = FPS{30, 1}
	FPS2997 = FPS{30000, 1001}
)

// Nominal returns the integer label rate (30 for 29.97).
func (f FPS) Nominal() int { return (f.Num + f.Den - 1) / f.Den }

// FramesPerDay returns timecode labels per 24h day.
func (f FPS) FramesPerDay() int64 { return int64(f.Nominal()) * 86400 }

// DayMs is the wrap period in milliseconds.
const DayMs = 86400_000

// ParseFPS parses "24", "25", "30", "29.97" or "30000/1001".
func ParseFPS(s string) (FPS, error) {
	switch s {
	case "24":
		return FPS24, nil
	case "25":
		return FPS25, nil
	case "30":
		return FPS30, nil
	case "29.97", "30000/1001":
		return FPS2997, nil
	}
	return FPS{}, model.Fatalf(model.StageTimecode, "unsupported fps %q", s)
}

var tcRe = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})([:;])(\d{2})$`)

// Parse converts HH:MM:SS:FF (or HH:MM:SS;FF for drop-frame) to a frame
// count since 00:00:00. drop must match the ';' separator and 29.97 fps;
// drop-frame-illegal labels are rejected.
func Parse(s string, fps FPS, drop bool) (int64, error) {
	m := tcRe.FindStringSubmatch(s)
	if m == nil {
		return 0, model.Fatalf(model.StageTimecode, "malformed timecode %q", s)
	}
	hh, _ := strconv.Atoi(m[1])
	mm, _ := strconv.Atoi(m[2])
	ss, _ := strconv.Atoi(m[3])
	ff, _ := strconv.Atoi(m[5])
	if hh > 23 || mm > 59 || ss > 59 {
		return 0, model.Fatalf(model.StageTimecode, "timecode %q out of range", s)
	}
	nom := fps.Nominal()
	if ff >= nom {
		return 0, model.Fatalf(model.StageTimecode, "timecode %q frame exceeds %d", s, nom)
	}
	isDrop := m[4] == ";"
	if isDrop != drop {
		return 0, model.Fatalf(model.StageTimecode,
			"timecode %q separator does not match drop=%v; refusing to guess", s, drop)
	}
	if drop && fps != FPS2997 {
		return 0, model.Fatalf(model.StageTimecode, "drop-frame requires 30000/1001 fps")
	}
	if drop {
		tm := hh*60 + mm
		frames := int64((tm*60+ss)*30+ff) - 2*int64(tm-tm/10)
		if mm%10 != 0 && ss == 0 && ff < 2 {
			return 0, model.Fatalf(model.StageTimecode, "timecode %q is a dropped label", s)
		}
		return frames, nil
	}
	return int64((hh*3600+mm*60+ss)*nom + ff), nil
}

// FramesToMs converts a frame count to media milliseconds (rounded).
func FramesToMs(frames int64, fps FPS) int64 {
	return floorDiv(frames*int64(fps.Den)*1000+int64(fps.Num)/2, int64(fps.Num))
}

// MsToFrames converts media milliseconds to the nearest frame count.
func MsToFrames(ms int64, fps FPS) int64 {
	return floorDiv(ms*int64(fps.Num)+int64(fps.Den)*500, int64(fps.Den)*1000)
}

// FrameDurMs returns the exact frame duration as a rational (num/den ms).
func FrameDurMs(fps FPS) (int64, int64) { return int64(fps.Den) * 1000, int64(fps.Num) }

// Format renders a frame count as a timecode label (inverse of Parse).
func Format(frames int64, fps FPS, drop bool) string {
	nom := int64(fps.Nominal())
	if !drop {
		fr := ((frames % (nom * 86400)) + nom*86400) % (nom * 86400)
		hh := fr / (nom * 3600)
		fr %= nom * 3600
		mm := fr / (nom * 60)
		fr %= nom * 60
		return fmt.Sprintf("%02d:%02d:%02d:%02d", hh, mm, fr/nom, fr%nom)
	}
	const (
		perMin   = 1798
		per10Min = 17982
		perHour  = 107892
		perDay   = 2589408
	)
	fr := ((frames % perDay) + perDay) % perDay
	hh := fr / perHour
	fr %= perHour
	mm := fr/per10Min*10
	m := fr % per10Min
	var rem int64
	if m < 1800 { // first minute of a 10-minute block drops nothing
		rem = m
	} else {
		rem = m - 1800
		mm += rem/perMin + 1
		rem %= perMin
	}
	if mm%10 != 0 {
		rem += 2 // labels 00 and 01 of second 0 do not exist here
	}
	ss, ff := rem/30, rem%30
	return fmt.Sprintf("%02d:%02d:%02d;%02d", hh, mm, ss, ff)
}

// dayFramesDF is the real frame count of one 24h drop-frame day.
const dayFramesDF = 2589408

// ResolveDaily maps a wrapped time-of-day frame count onto the unified
// timeline near refMs, unwrapping across midnight. The wrap period is
// the real day length (drop-frame days are ~86ms shorter than 24h). A
// label landing exactly half a day from the reference is ambiguous and
// rejected.
func ResolveDaily(frames int64, fps FPS, drop bool, refMs int64) (model.MediaMs, error) {
	dayFrames := fps.FramesPerDay()
	if drop {
		dayFrames = dayFramesDF
	}
	dayMs := FramesToMs(dayFrames, fps)
	base := FramesToMs(frames, fps) // 0 <= base < dayMs
	k := floorDiv(refMs-base+dayMs/2, dayMs)
	cand := base + k*dayMs
	diff := cand - refMs
	if diff < 0 {
		diff = -diff
	}
	if 2*diff == dayMs {
		return 0, model.Fatalf(model.StageTimecode,
			"timecode is exactly 12h from reference %dms; ambiguous wrap", refMs)
	}
	return cand, nil
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
