// Package clock is the single time authority for the archive pipeline.
//
// All wall-clock quantities are represented as Tick values on a fixed
// 30000 Hz grid. Frame durations of 24, 25, 30 and 30000/1001 fps are whole
// numbers of ticks (1250, 1200, 1000 and 1001), so every conversion below
// is exact integer arithmetic; no floating point is ever accumulated.
package clock

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TickRate is the number of archive ticks in one wall-clock second.
const TickRate int64 = 30000

// TickPerDay is the grid length of one 24-hour day.
const TickPerDay = TickRate * 86400

// MaxTick bounds tick arithmetic to roughly 9.7 million years while keeping
// products in int64 regardless of the rational numerators involved.
const MaxTick int64 = 1 << 62

// Tick is an instant or duration on the archive 30 kHz grid.
type Tick int64

// Rat is an exact, intentionally non-normalised rational number.
type Rat struct{ Num, Den int64 }

var (
	errBadRate  = errors.New("frame rate must be positive and within int range")
	errInexact  = errors.New("rate does not divide the archive tick grid exactly")
	errBadClock = errors.New("media clock rate must be positive")
	errOverflow = errors.New("value crosses an allowed boundary")
)

// Rate describes a frame rate as Num frames per Den seconds. Drop marks the
// SMPTE drop-frame counting convention (only meaningful for 30000/1001).
type Rate struct {
	Num  int64
	Den  int64
	Drop bool
}

// Standard rate presets.
var (
	Rate24     = Rate{Num: 24, Den: 1}
	Rate25     = Rate{Num: 25, Den: 1}
	Rate30     = Rate{Num: 30, Den: 1}
	Rate2997   = Rate{Num: 30000, Den: 1001}
	Rate2997DF = Rate{Num: 30000, Den: 1001, Drop: true}
)

// FPS returns the rate as frames/second.
func (r Rate) FPS() Rat { return Rat{Num: r.Num, Den: r.Den} }

// NominalFPS is the integer frame count used per second by timecode labels,
// i.e. 30 for 30000/1001, 24 for 24 fps and so on.
func (r Rate) NominalFPS() int64 { return (r.Num + r.Den - 1) / r.Den }

// ParseRate accepts "24", "30" or "30000/1001".
func ParseRate(s string) (Rate, error) {
	s = strings.TrimSpace(s)
	var num, den int64
	var err error
	if i := strings.IndexByte(s, '/'); i >= 0 {
		if num, err = strconv.ParseInt(s[:i], 10, 64); err != nil {
			return Rate{}, fmt.Errorf("%w: %q", errBadRate, s)
		}
		if den, err = strconv.ParseInt(s[i+1:], 10, 64); err != nil {
			return Rate{}, fmt.Errorf("%w: %q", errBadRate, s)
		}
	} else {
		if num, err = strconv.ParseInt(s, 10, 64); err != nil {
			return Rate{}, fmt.Errorf("%w: %q", errBadRate, s)
		}
		den = 1
	}
	if num <= 0 || den <= 0 || num > 1<<31 || den > 1<<31 {
		return Rate{}, fmt.Errorf("%w: %q", errBadRate, s)
	}
	return Rate{Num: num, Den: den}, nil
}

// FrameDurTicks returns the exact duration of one frame in archive ticks.
// Rates whose frame length does not land on the grid are rejected rather
// than silently rounded.
func (r Rate) FrameDurTicks() (int64, error) {
	num := TickRate * r.Den
	if num%r.Num != 0 {
		return 0, fmt.Errorf("%w: %d/%d", errInexact, r.Num, r.Den)
	}
	return num / r.Num, nil
}

// FrameToTick converts a zero-based frame number to an instant.
func FrameToTick(r Rate, frame int64) (Tick, error) {
	if frame < 0 {
		return 0, fmt.Errorf("negative frame number %d", frame)
	}
	dur, err := r.FrameDurTicks()
	if err != nil {
		return 0, err
	}
	t := Tick(frame * dur)
	if int64(t) > MaxTick {
		return 0, fmt.Errorf("frame %d overflows the time grid", frame)
	}
	return t, nil
}

// TickToFrame floors an instant to its containing frame number.
func TickToFrame(r Rate, t Tick) (int64, error) {
	if t < 0 {
		return 0, fmt.Errorf("negative tick %d", t)
	}
	dur, err := r.FrameDurTicks()
	if err != nil {
		return 0, err
	}
	return int64(t) / dur, nil
}

// MillisToTick maps a millisecond instant onto the grid (exact: 30 ticks/ms).
func MillisToTick(ms int64) Tick { return Tick(ms * 30) }

// RoundMillis renders a tick value as whole milliseconds, rounding to nearest.
func (t Tick) RoundMillis() int64 {
	v := int64(t)
	if v >= 0 {
		return (v + 15) / 30
	}
	return -((-v + 15) / 30)
}

// WrapDay folds an instant into [0, TickPerDay) for day-boundary rollover.
func WrapDay(t Tick) Tick {
	n := int64(t) % TickPerDay
	if n < 0 {
		n += TickPerDay
	}
	return Tick(n)
}

// ratExact converts value*num/den, requiring an integral result.
func ratExact(value, num, den int64) (int64, error) {
	if den <= 0 || value < 0 {
		return 0, errBadClock
	}
	prod := value * num
	if prod/num != value { // int64 overflow guard
		return 0, fmt.Errorf("media timestamp %d overflows the time grid", value)
	}
	if prod%den != 0 {
		return 0, fmt.Errorf("timestamp %d on clock /%d is not grid-aligned", value, den)
	}
	return prod / den, nil
}

// RTPToTick maps an RTP timestamp on clockRate Hz (typically 90000) to the
// archive grid. 90 kHz values land exactly every three ticks.
func RTPToTick(ts uint32, clockRate uint32) (Tick, error) {
	if clockRate == 0 {
		return 0, errBadClock
	}
	v, err := ratExact(int64(ts), TickRate, int64(clockRate))
	if err != nil {
		return 0, fmt.Errorf("rtp: %w", err)
	}
	return Tick(v), nil
}

// TickToRTP is the inverse used when emitting extension timestamps.
func TickToRTP(t Tick, clockRate uint32) (uint32, error) {
	if clockRate == 0 || t < 0 {
		return 0, errBadClock
	}
	v, err := ratExact(int64(t), int64(clockRate), TickRate)
	if err != nil {
		return 0, fmt.Errorf("rtp: %w", err)
	}
	return uint32(v), nil
}

// MP4ToTick maps an MP4 timescale sample number to the archive grid.
func MP4ToTick(sampleDelta int64, timescale uint32) (Tick, error) {
	if timescale == 0 {
		return 0, errBadClock
	}
	v, err := ratExact(sampleDelta, TickRate, int64(timescale))
	if err != nil {
		return 0, fmt.Errorf("mp4: %w", err)
	}
	return Tick(v), nil
}

// TickToMP4 is the inverse mapping for a given MP4 timescale.
func TickToMP4(t Tick, timescale uint32) (int64, error) {
	if timescale == 0 || t < 0 {
		return 0, errBadClock
	}
	return ratExact(int64(t), int64(timescale), TickRate)
}
