package format

import "medialab/internal/clock"

// Timebase returns the rational duration of one media tick.
func (t *Track) Timebase() clock.Rational {
	scale := int64(t.Timescale)
	if scale == 0 {
		scale = 1
	}
	r, _ := clock.Rat(1, scale)
	return r
}

// DurationTime returns the track duration on the unified rational clock.
func (t *Track) DurationTime() clock.Time {
	return clock.Time{Ticks: int64(t.Duration), Base: t.Timebase()}
}
