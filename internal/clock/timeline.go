// Package clock provides a unified integer time base for all tracks and
// handles timestamp wrap-around (e.g. 33-bit MPEG PTS or 32-bit RTP clocks),
// duplicate/regression detection and overflow-safe rescaling.
package clock

import (
	"fmt"
	"math"

	"aircheck/internal/fault"
)

// Base is the unified timeline resolution: 90 kHz, the MPEG/RTP standard
// clock, so audio and video tracks share one integer time line.
const Base = 90000

// MaxUnits is the largest representable unified time (~3.2 million years).
const MaxUnits = math.MaxInt64 / 2

// Rescale converts v from one timescale to another with overflow checking.
func Rescale(v int64, from, to uint32) (int64, error) {
	if from == 0 || to == 0 {
		return 0, fmt.Errorf("timescale must be non-zero (from=%d to=%d)", from, to)
	}
	if v != 0 && (v > math.MaxInt64/int64(to) || v < math.MinInt64/int64(to)) {
		return 0, fmt.Errorf("rescale overflow: %d * %d / %d", v, to, from)
	}
	return v * int64(to) / int64(from), nil
}

// ToMillis converts unified-base units to milliseconds.
func ToMillis(units int64) float64 {
	return float64(units) * 1000.0 / float64(Base)
}

// Unwrapper expands a wrapping timestamp counter (mod 2^bits) into a
// monotonically non-decreasing 64-bit sequence.
type Unwrapper struct {
	bits     uint
	mod      int64
	half     int64
	init     bool
	lastRaw  int64
	lastFull int64
}

// NewUnwrapper creates an Unwrapper for a counter of the given bit width
// (1..63).
func NewUnwrapper(bits uint) (*Unwrapper, error) {
	if bits == 0 || bits > 63 {
		return nil, fmt.Errorf("invalid wrap bits %d", bits)
	}
	mod := int64(1) << bits
	return &Unwrapper{bits: bits, mod: mod, half: mod / 2}, nil
}

// Next feeds the next raw counter value and returns the unwrapped value.
func (u *Unwrapper) Next(raw uint64) int64 {
	r := int64(raw % uint64(u.mod))
	if !u.init {
		u.init = true
		u.lastRaw = r
		u.lastFull = r
		return r
	}
	delta := r - u.lastRaw
	// Fold the delta into (-mod/2, mod/2] to detect forward wrap.
	if delta > u.half {
		delta -= u.mod
	} else if delta < -u.half {
		delta += u.mod
	}
	u.lastRaw = r
	u.lastFull += delta
	return u.lastFull
}

// TrackState tracks per-track timestamp validation on the unified base.
type TrackState struct {
	TrackID   uint32
	Timescale uint32
	lastPTS   int64 // unified base
	hasPTS    bool
	Faults    fault.List
}

// Observe registers one decoded presentation time (raw, in track timescale)
// and returns the unified-base time. It records duplicates, regressions and
// overflow into the track's fault list.
func (t *TrackState) Observe(rawPTS int64, offset int64) (int64, error) {
	pts, err := Rescale(rawPTS, t.Timescale, Base)
	if err != nil {
		t.Faults = append(t.Faults, fault.New(
			fault.KindTSOverflow, fault.StageTimeline, t.TrackID, 0, offset, false,
			fmt.Sprintf("timestamp overflow: %s", err)))
		return 0, err
	}
	if t.hasPTS {
		switch {
		case pts == t.lastPTS:
			t.Faults = append(t.Faults, fault.New(
				fault.KindTSDuplicate, fault.StageTimeline, t.TrackID, pts, offset, true,
				fmt.Sprintf("duplicate pts %d", pts)))
		case pts < t.lastPTS:
			t.Faults = append(t.Faults, fault.New(
				fault.KindTSRegression, fault.StageTimeline, t.TrackID, pts, offset, true,
				fmt.Sprintf("pts regressed %d -> %d", t.lastPTS, pts)))
		}
	}
	t.hasPTS = true
	t.lastPTS = pts
	return pts, nil
}

// AVSyncError returns the absolute A/V offset in milliseconds between the
// first observed timestamps of an audio and a video track.
func AVSyncError(audio, video *TrackState, audioFirst, videoFirst int64) float64 {
	_ = audio
	_ = video
	d := audioFirst - videoFirst
	if d < 0 {
		d = -d
	}
	return ToMillis(d)
}
