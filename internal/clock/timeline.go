// Package clock provides a unified 90 kHz media time base shared by all
// tracks, conversion from per-track clock rates (e.g. 48 kHz audio
// samples), and 32-bit RTP timestamp wraparound unfolding.
package clock

import "fmt"

// MediaClockRate is the common media timeline rate (ticks per second).
const MediaClockRate = 90000

// UnknownTrackError is returned when a timestamp is presented for a track
// that was never registered. It carries the track name so callers can
// locate the offending input.
type UnknownTrackError struct {
	Track string
}

func (e *UnknownTrackError) Error() string {
	return fmt.Sprintf("clock: unknown track %q", e.Track)
}

// InvalidTimebaseError is returned when a track is registered with an
// unusable clock rate (zero, or above the media clock rate).
type InvalidTimebaseError struct {
	Track string
	Rate  uint32
}

func (e *InvalidTimebaseError) Error() string {
	return fmt.Sprintf("clock: invalid timebase %d for track %q", e.Rate, e.Track)
}

type trackState struct {
	rate    uint32
	inited  bool
	lastRaw uint32
	lastExt int64 // extended (wrap-unfolded) timestamp in track clock units
}

// Timeline tracks registered media tracks and converts their native
// 32-bit timestamps onto the shared 90 kHz media timeline.
type Timeline struct {
	tracks map[string]*trackState
}

// New returns an empty Timeline.
func New() *Timeline {
	return &Timeline{tracks: make(map[string]*trackState)}
}

// Register adds a track with its native clock rate. Audio tracks typically
// use their sample rate (e.g. 48000); video uses 90000.
func (t *Timeline) Register(track string, rate uint32) error {
	if rate == 0 || rate > MediaClockRate {
		return &InvalidTimebaseError{Track: track, Rate: rate}
	}
	t.tracks[track] = &trackState{rate: rate}
	return nil
}

// Rate returns the native clock rate of a registered track.
func (t *Timeline) Rate(track string) (uint32, error) {
	st, ok := t.tracks[track]
	if !ok {
		return 0, &UnknownTrackError{Track: track}
	}
	return st.rate, nil
}

// Unwrap folds a 32-bit track timestamp into a monotonically increasing
// 64-bit value in track clock units, resolving wraparound by choosing the
// continuation nearest to the previous value.
func (t *Timeline) Unwrap(track string, raw uint32) (int64, error) {
	st, ok := t.tracks[track]
	if !ok {
		return 0, &UnknownTrackError{Track: track}
	}
	if !st.inited {
		st.inited = true
		st.lastRaw = raw
		st.lastExt = int64(raw)
		return st.lastExt, nil
	}
	delta := int32(raw - st.lastRaw) // signed delta handles wrap
	st.lastExt += int64(delta)
	st.lastRaw = raw
	return st.lastExt, nil
}

// ToMedia converts a raw 32-bit track timestamp to the 90 kHz media
// timeline, unfolding wraparound in the track's native units first.
func (t *Timeline) ToMedia(track string, raw uint32) (int64, error) {
	st, ok := t.tracks[track]
	if !ok {
		return 0, &UnknownTrackError{Track: track}
	}
	ext, err := t.Unwrap(track, raw)
	if err != nil {
		return 0, err
	}
	return ext * int64(MediaClockRate) / int64(st.rate), nil
}

// SamplesToMedia converts a count of audio samples at rate to 90 kHz
// media ticks.
func SamplesToMedia(samples int64, rate uint32) int64 {
	return samples * int64(MediaClockRate) / int64(rate)
}

// MediaToMillis converts 90 kHz media ticks to milliseconds.
func MediaToMillis(ticks int64) float64 {
	return float64(ticks) * 1000.0 / float64(MediaClockRate)
}

// MillisToMedia converts milliseconds to 90 kHz media ticks.
func MillisToMedia(ms float64) int64 {
	return int64(ms * float64(MediaClockRate) / 1000.0)
}
