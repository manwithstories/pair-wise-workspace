// Package timeline unifies container time, decode order and program time.
// clock.go converts between track timescale ticks, movie-timescale edit
// list segments and program time (microseconds). All conversions are
// overflow-checked and rounded to the nearest microsecond, keeping the
// error well under the 1ms acceptance bound.
package timeline

import (
	"math/bits"

	"archive/internal/manifest"
	"archive/internal/media"
)

// MicrosPerSecond is the program-time resolution.
const MicrosPerSecond = 1_000_000

// Loc carries error context (track + input offset) into the clock math.
type Loc struct {
	Track  string
	Offset int64
}

// TicksToMicros converts track ticks to microseconds, rounding to nearest.
// A multiplication overflow returns a locatable error instead of wrapping.
func TicksToMicros(ticks int64, timescale uint32, loc Loc) (int64, error) {
	if timescale == 0 {
		return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset, "zero timescale")
	}
	neg := ticks < 0
	u := uint64(ticks)
	if neg {
		u = uint64(-ticks)
	}
	hi, lo := bits.Mul64(u, MicrosPerSecond)
	if hi != 0 {
		return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset,
			"multiplication overflow in ticks->micros")
	}
	q := (lo + uint64(timescale)/2) / uint64(timescale)
	if q > uint64(1<<62) {
		return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset,
			"timestamp out of representable range")
	}
	v := int64(q)
	if neg {
		v = -v
	}
	return v, nil
}

// MicrosToTicks converts microseconds to track ticks, rounding to nearest.
func MicrosToTicks(us int64, timescale uint32, loc Loc) (int64, error) {
	if timescale == 0 {
		return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset, "zero timescale")
	}
	neg := us < 0
	u := uint64(us)
	if neg {
		u = uint64(-us)
	}
	hi, lo := bits.Mul64(u, uint64(timescale))
	if hi != 0 {
		return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset,
			"multiplication overflow in micros->ticks")
	}
	q := (lo + MicrosPerSecond/2) / MicrosPerSecond
	v := int64(q)
	if neg {
		v = -v
	}
	return v, nil
}

// MediaToProgram maps a media-time tick through the edit list to program
// time in microseconds. Empty edits (MediaTime == -1) contribute dwell.
// A media time not covered by any edit is an out-of-range edit list error.
func MediaToProgram(mediaTicks int64, timescale uint32, edits []manifest.RawEdit,
	movieTimescale uint32, loc Loc) (int64, error) {
	if len(edits) == 0 {
		// No edit list: program time equals media time.
		return TicksToMicros(mediaTicks, timescale, loc)
	}
	var programBase int64 // accumulated program µs before current edit
	for i, e := range edits {
		dwell, err := TicksToMicros(e.SegmentDuration, movieTimescale,
			Loc{Track: loc.Track, Offset: int64(i)})
		if err != nil {
			return 0, err
		}
		if e.MediaTime == -1 {
			programBase += dwell
			continue
		}
		if mediaTicks < e.MediaTime {
			return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset,
				"media time before first covering edit (edit list out of range)")
		}
		delta, err := toMicrosChecked(mediaTicks-e.MediaTime, timescale, loc)
		if err != nil {
			return 0, err
		}
		return programBase + delta, nil
	}
	return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset,
		"media time beyond last edit (edit list out of range)")
}

// EditEnd returns the media-time tick covered by the edit list's last
// entry, used to validate that an edit list does not run past the media.
func EditEnd(edits []manifest.RawEdit, timescale, movieTimescale uint32, loc Loc) (int64, error) {
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		if e.MediaTime == -1 {
			continue
		}
		dwellUs, err := TicksToMicros(e.SegmentDuration, movieTimescale,
			Loc{Track: loc.Track, Offset: int64(i)})
		if err != nil {
			return 0, err
		}
		durTicks, err := MicrosToTicks(dwellUs, timescale, loc)
		if err != nil {
			return 0, err
		}
		return e.MediaTime + durTicks, nil
	}
	return 0, nil
}

func toMicrosChecked(ticks int64, timescale uint32, loc Loc) (int64, error) {
	if ticks < 0 {
		return 0, media.Corruptf("timeline.clock", loc.Track, loc.Offset,
			"negative duration in edit mapping")
	}
	return TicksToMicros(ticks, timescale, loc)
}
