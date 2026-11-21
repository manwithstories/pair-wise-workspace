package timeline

import (
	"fmt"
	"strconv"
	"strings"

	"mediacheck/internal/fault"
)

// Timecode is a parsed SMPTE timecode in the shared nanosecond base.
type Timecode struct {
	Nanos     int64 // absolute position on the program timeline
	Frame     int64 // absolute frame count at the given rate
	DropFrame bool
	Raw       string // original text, preserved for reporting
}

// ParseTimecode parses "HH:MM:SS:FF" (non-drop) or "HH:MM:SS;FF"
// (drop-frame). srcOffset is the byte offset of the timecode in its
// source, embedded in any error so faults are locatable.
//
// Drop-frame is only defined for 30000/1001; a ';' separator with any
// other rate is ambiguous and rejected with a locatable error. Frame
// numbers must be below the nominal FPS. Hours may exceed 23 (the
// caller applies cross-day wrap via WrapDay when needed).
func ParseTimecode(s string, rate Rate, srcOffset int64) (Timecode, error) {
	raw := s
	s = strings.TrimSpace(s)
	drop := strings.Contains(s, ";")
	sep := ":"
	if drop {
		sep = ";"
	}
	// Split HH:MM:SS then FF on the last separator.
	main := s
	ffStr := ""
	if i := strings.LastIndex(s, sep); i >= 0 {
		main, ffStr = s[:i], s[i+1:]
	}
	parts := strings.Split(main, ":")
	if len(parts) != 3 || ffStr == "" {
		return Timecode{}, fault.New(fault.CatTimecode, srcOffset, false,
			"malformed timecode %q", raw)
	}
	field := func(name, v string) (int64, error) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, fault.New(fault.CatTimecode, srcOffset, false,
				"bad %s field in %q", name, raw)
		}
		return n, nil
	}
	hh, err := field("hour", parts[0])
	if err != nil {
		return Timecode{}, err
	}
	mm, err := field("minute", parts[1])
	if err != nil {
		return Timecode{}, err
	}
	ss, err := field("second", parts[2])
	if err != nil {
		return Timecode{}, err
	}
	ff, err := field("frame", ffStr)
	if err != nil {
		return Timecode{}, err
	}
	if mm > 59 || ss > 59 {
		return Timecode{}, fault.New(fault.CatTimecode, srcOffset, false,
			"minute/second out of range in %q", raw)
	}
	nominal := rate.NominalFPS()
	if ff >= nominal {
		return Timecode{}, fault.New(fault.CatTimecode, srcOffset, false,
			"illegal frame number %d (nominal fps %d) in %q", ff, nominal, raw)
	}
	if drop && !rate.DropFrameAllowed() {
		return Timecode{}, fault.New(fault.CatTimecode, srcOffset, false,
			"drop-frame separator with rate %d/%d is ambiguous in %q",
			rate.Num, rate.Den, raw)
	}

	var frame int64
	if drop {
		frame = dropFrameCount(hh, mm, ss, ff, nominal, srcOffset, raw)
		if frame < 0 {
			return Timecode{}, fault.New(fault.CatTimecode, srcOffset, false,
				"frame number %d does not exist in drop-frame %q", ff, raw)
		}
	} else {
		frame = ((hh*60+mm)*60+ss)*nominal + ff
	}
	return Timecode{
		Nanos:     rate.FrameToNanos(frame),
		Frame:     frame,
		DropFrame: drop,
		Raw:       raw,
	}, nil
}

// dropFrameCount converts HH:MM:SS;FF to an absolute frame count using
// the SMPTE 2-frame drop rule; returns -1 for dropped (nonexistent)
// frame numbers 00 and 01 at non-tenth minute starts.
func dropFrameCount(hh, mm, ss, ff, nominal, srcOffset int64, raw string) int64 {
	totalMinutes := hh*60 + mm
	dropped := int64(2) * (totalMinutes - totalMinutes/10)
	frame := ((hh*60+mm)*60+ss)*nominal + ff - dropped
	if ss == 0 && ff < 2 && mm%10 != 0 {
		return -1 // dropped frame number: does not exist
	}
	return frame
}

// Format renders nanoseconds as a timecode string at the given rate,
// using ';' when drop is requested. Inverse of ParseTimecode for
// in-range values, used by round-trip verification.
func Format(nanos int64, rate Rate, drop bool) string {
	frame := rate.NanosToFrame(nanos)
	nominal := rate.NominalFPS()
	if drop {
		// Re-add dropped frame numbers for display.
		totalFrames := frame
		framesPer10Min := nominal*600 - 18 // 17982 at 30fps
		framesPerMin := nominal*60 - 2
		d := totalFrames / framesPer10Min
		m := totalFrames % framesPer10Min
		extra := int64(0)
		if m >= nominal*60 {
			extra = 2 + 2*((m-nominal*60)/framesPerMin)
		}
		frame = totalFrames + 18*d + extra
	}
	fps := nominal
	ff := frame % fps
	totalSec := frame / fps
	ss := totalSec % 60
	mm := (totalSec / 60) % 60
	hh := totalSec / 3600
	sep := ":"
	if drop {
		sep = ";"
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%02d", hh, mm, ss, sep, ff)
}
