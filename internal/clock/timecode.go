package clock

import (
	"fmt"
	"strconv"
	"strings"
)

// Timecode is a SMPTE ST 12-1 style time code address. Drop marks whether
// the address uses drop-frame counting (only legal at 29.97 fps).
type Timecode struct {
	Hour     int
	Minute   int
	Second   int
	Frame    int
	Drop     bool
	Rate     Rate
}

// frameCount converts a time code address to the elapsed frame number since
// midnight. The arithmetic is the integer SMPTE formula; no float accumulation.
func (tc Timecode) frameCount() (int64, error) {
	r := tc.Rate
	if !r.Valid() {
		return 0, fmt.Errorf("clock: timecode has no valid rate")
	}
	if tc.Drop && (r.Num != 30000 || r.Den != 1001) {
		return 0, fmt.Errorf("clock: drop-frame only defined for 30000/1001, got %d/%d", r.Num, r.Den)
	}
	if tc.Hour < 0 || tc.Hour > 23 || tc.Minute < 0 || tc.Minute > 59 ||
		tc.Second < 0 || tc.Second > 59 || tc.Frame < 0 ||
		tc.Frame >= r.NominalFPS() {
		return 0, fmt.Errorf("clock: timecode field out of range %02d:%02d:%02d:%02d",
			tc.Hour, tc.Minute, tc.Second, tc.Frame)
	}
	hh, mm, ss, ff := int64(tc.Hour), int64(tc.Minute), int64(tc.Second), int64(tc.Frame)
	nominal := int64(r.NominalFPS()) // displayed frame numbers per second (30/25/24)
	total := (hh*3600+mm*60+ss)*nominal + ff
	if tc.Drop {
		// SMPTE ST 12-1: two frame numbers are skipped at the start of
		// every minute except every tenth. Each hour therefore contains
		// six groups of nine drop minutes = 54 drop minutes.
		dropFrames := nominal / 15
		dropMinutes := 54*hh + mm - mm/10
		total -= dropFrames * dropMinutes
	}
	return total, nil
}

// Ticks converts the address to the shared 90 kHz time base.
func (tc Timecode) Ticks() (int64, error) {
	frames, err := tc.frameCount()
	if err != nil {
		return 0, err
	}
	return tc.Rate.FrameDuration(frames), nil
}

// String renders the canonical HH:MM:SS:FF form, using ';' before the frame
// field (and '.' between other fields) for drop-frame addresses.
func (tc Timecode) String() string {
	sep := byte(':')
	if tc.Drop {
		sep = ';'
	}
	return fmt.Sprintf("%02d%c%02d%c%02d%c%02d",
		tc.Hour, sep, tc.Minute, sep, tc.Second, sep, tc.Frame)
}

// ParseTimecode reads HH:MM:SS:FF or the drop spellings HH:MM:SS;FF and
// HH;MM;SS;FF against the supplied rate.
func ParseTimecode(s string, r Rate) (Timecode, error) {
	if !r.Valid() {
		return Timecode{}, fmt.Errorf("clock: invalid rate for timecode %q", s)
	}
	drop := strings.ContainsAny(s, ";")
	parts := strings.FieldsFunc(s, func(c rune) bool { return c == ':' || c == ';' || c == '.' })
	if len(parts) != 4 {
		return Timecode{}, fmt.Errorf("clock: malformed timecode %q", s)
	}
	vals := make([]int, 4)
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil {
			return Timecode{}, fmt.Errorf("clock: non-numeric timecode field %q", p)
		}
		vals[i] = v
	}
	tc := Timecode{Hour: vals[0], Minute: vals[1], Second: vals[2], Frame: vals[3], Drop: drop, Rate: r}
	if _, err := tc.frameCount(); err != nil {
		return Timecode{}, err
	}
	return tc, nil
}

// FromFrames builds a time code address from an elapsed frame number, applying
// day-boundary wrap-around and inverse drop-frame adjustment. Negative frame
// counts wrap backwards across midnight.
func FromFrames(frames int64, r Rate, drop bool) (Timecode, error) {
	if !r.Valid() {
		return Timecode{}, fmt.Errorf("clock: invalid rate")
	}
	if drop && (r.Num != 30000 || r.Den != 1001) {
		return Timecode{}, fmt.Errorf("clock: drop-frame only defined for 30000/1001")
	}
	perDay := r.FramesPerDay()
	frames = frames % perDay // day-boundary wrap, works for negatives in Go
	if frames < 0 {
		frames += perDay
	}
	nominal := int64(r.NominalFPS())
	if drop {
		// Inverse of the ST 12-1 skip. Each drop minute contains
		// (60*nominal - dropFrames) real frames; its address runs from
		// :02 up to the next minute's :00. Count whole drop minutes
		// before the target and re-add their skipped frame numbers.
		dropFrames := nominal / 15
		framesPer10Min := nominal*60*10 - 9*dropFrames
		framesPerDropMin := nominal*60 - dropFrames
		block := frames / framesPer10Min
		within := frames % framesPer10Min
		dropMinBefore := block*9
		if within > dropFrames {
			dropMinBefore += (within - dropFrames) / framesPerDropMin
		}
		frames += dropFrames * dropMinBefore
	}
	ff := frames % nominal
	s := (frames / nominal) % 60
	m := (frames / (nominal * 60)) % 60
	h := frames / (nominal * 3600)
	return Timecode{Hour: int(h), Minute: int(m), Second: int(s), Frame: int(ff), Drop: drop, Rate: r}, nil
}

// Shift moves a time code by a signed frame offset, wrapping at midnight for
// positive and negative offsets alike.
func (tc Timecode) Shift(offsetFrames int64) (Timecode, error) {
	frames, err := tc.frameCount()
	if err != nil {
		return Timecode{}, err
	}
	return FromFrames(frames+offsetFrames, tc.Rate, tc.Drop)
}
