// Package clock defines the project-wide 90 kHz media time base and the
// primitive operations every other subsystem shares: unit conversion,
// 32/33-bit timestamp unwrapping and linear node-clock drift correction.
//
// All timestamps inside fieldcut are expressed in Ticks on a single
// monotonically increasing 90 kHz axis. RTP/MPEG-TS wire values are 32 or
// 33 bits wide and are unwrapped against the last seen value before they
// enter the timeline.
package clock

import (
	"errors"
	"math"
	"time"
)

// Rate is the common media clock rate in ticks per second (MPEG-TS /
// MPEG-2 Systems 90 kHz clock).
const Rate int64 = 90000

// Max32 is the modulo of a 32-bit RTP timestamp field.
const Max32 int64 = 1 << 32

// Max33 is the modulo of a 33-bit MPEG-TS PCR/PTS field.
const Max33 int64 = 1 << 33

// Safety bounds. Every input size, track count and look-back window in the
// project is capped so adversarial manifests or streams cannot exhaust
// memory. They live here because timeline/index code shares them.
const (
	// MaxTracks caps the number of tracks accepted from one manifest.
	MaxTracks = 64
	// MaxManifestSegments caps entries accepted in a single playlist.
	MaxManifestSegments = 100_000
	// MaxLateWindowTicks is the hard ceiling for the replay look-back
	// window (300 s on the 90 kHz clock).
	MaxLateWindowTicks = 300 * Rate
	// MaxDriftPPM rejects drift slopes beyond a physically plausible
	// node oscillator error (100 000 PPM == 10%).
	MaxDriftPPM = 100_000
)

// Sentinel errors shared across subsystems. Wrapped with fmt.Errorf %w at
// call sites so errors.Is keeps working.
var (
	ErrInvalidArgument = errors.New("clock: invalid argument")
	ErrUntrusted       = errors.New("clock: timestamp range cannot be uniquely inferred")
	ErrWrapOverflow    = errors.New("clock: timestamp wrap-around overflow")
	ErrDriftExceeded   = errors.New("clock: node clock drift exceeds allowed bound")
	ErrPathTraversal   = errors.New("clock: manifest path escapes base directory")
	ErrDuplicateSeq    = errors.New("clock: conflicting duplicate segment sequence")
	ErrManifestTooLarge = errors.New("clock: manifest exceeds configured size limit")
	ErrTruncatedBox    = errors.New("clock: truncated or malformed mp4 box")
	ErrGap             = errors.New("clock: unrecoverable packet gap")
	ErrQueueFull       = errors.New("clock: bounded queue is full")
	ErrQueueClosed     = errors.New("clock: queue already closed")
	ErrCheckpoint      = errors.New("clock: checkpoint cannot be resumed")
)

// Ticks is a duration on the unified 90 kHz timeline.
type Ticks int64

// FromDuration converts a standard library duration to 90 kHz ticks,
// rounding to the nearest tick.
func FromDuration(d time.Duration) Ticks {
	return Ticks(math.Round(float64(d) / float64(time.Second) * float64(Rate)))
}

// Duration converts ticks back to a time.Duration.
func (t Ticks) Duration() time.Duration {
	return time.Duration(math.Round(float64(t) * float64(time.Second) / float64(Rate)))
}

// Milliseconds reports the tick count in whole milliseconds (rounded).
func (t Ticks) Milliseconds() int64 {
	return int64(math.Round(float64(t) * 1000 / float64(Rate)))
}

// Seconds reports the tick count in fractional seconds.
func (t Ticks) Seconds() float64 {
	return float64(t) / float64(Rate)
}

// Add returns t+d.
func (t Ticks) Add(d Ticks) Ticks { return t + d }

// Delta32 returns the signed shortest forward distance from prev (a
// previously unwrapped tick) to raw (a fresh 32-bit wire value), assuming
// no more than one wrap between consecutive samples. A backward jump larger
// than half the modulus is treated as a wrap; a genuine backward jump
// smaller than that is returned as a negative delta so callers can detect
// timestamp regression.
func Delta32(prev Ticks, raw uint32) int64 {
	mod := Max32
	cur := int64(raw)
	last := int64(prev) % mod
	if last < 0 {
		last += mod
	}
	d := cur - last
	if d <= -mod/2 {
		d += mod
	} else if d > mod/2 {
		d -= mod
	}
	return d
}

// Unwrap32 maps a sequence of 32-bit wire timestamps onto a monotonic
// 64-bit axis. base is the unwrapped predecessor; the result is base plus
// the signed shortest delta. It implements the RTP (32-bit) case.
func Unwrap32(base Ticks, raw uint32) Ticks {
	return Ticks(int64(base) + Delta32(base, raw))
}

// Delta33 is the 33-bit variant used for MPEG-TS PTS/DTS style fields.
func Delta33(prev Ticks, raw uint64) int64 {
	mod := Max33
	cur := int64(raw & uint64(mod-1))
	last := int64(prev) % mod
	if last < 0 {
		last += mod
	}
	d := cur - last
	if d <= -mod/2 {
		d += mod
	} else if d > mod/2 {
		d -= mod
	}
	return d
}

// Unwrap33 maps 33-bit wire values onto the monotonic axis.
func Unwrap33(base Ticks, raw uint64) Ticks {
	return Ticks(int64(base) + Delta33(base, raw))
}

// DriftEstimator fits the affine relation corrected = slope*raw + offset
// between a node's local timestamps and the reference timeline, using two
// or more anchor pairs. With a single anchor it performs offset-only
// correction (slope 1). Estimators are deterministic: identical anchors
// always yield identical coefficients.
type DriftEstimator struct {
	sumX, sumY, sumXY, sumXX float64
	n                        int
	maxPPM                   int64
}

// NewDriftEstimator builds an estimator. maxPPM <= 0 selects MaxDriftPPM.
func NewDriftEstimator(maxPPM int64) *DriftEstimator {
	if maxPPM <= 0 {
		maxPPM = MaxDriftPPM
	}
	return &DriftEstimator{maxPPM: maxPPM}
}

// Anchor records one (local raw tick, reference tick) correspondence.
func (e *DriftEstimator) Anchor(raw, ref Ticks) {
	x := float64(raw)
	y := float64(ref)
	e.n++
	e.sumX += x
	e.sumY += y
	e.sumXY += x * y
	e.sumXX += x * x
}

// N reports the number of anchors consumed.
func (e *DriftEstimator) N() int { return e.n }

// Fit returns slope and intercept. With fewer than two anchors it returns
// slope 1 and an intercept that pins the anchor (if any) to its reference.
// A slope outside the configured PPM envelope returns ErrDriftExceeded.
func (e *DriftEstimator) Fit() (slope float64, intercept Ticks, err error) {
	if e.n == 0 {
		return 1, 0, nil
	}
	if e.n == 1 {
		// intercept = ref - raw
		return 1, Ticks(e.sumY - e.sumX), nil
	}
	d := float64(e.n)*e.sumXX - e.sumX*e.sumX
	if d == 0 {
		return 1, Ticks(e.sumY/float64(e.n) - e.sumX/float64(e.n)), nil
	}
	slope = (float64(e.n)*e.sumXY - e.sumX*e.sumY) / d
	ppm := math.Abs(slope-1) * 1_000_000
	if ppm > float64(e.maxPPM) {
		return 0, 0, ErrDriftExceeded
	}
	icpt := (e.sumY - slope*e.sumX) / float64(e.n)
	return slope, Ticks(math.Round(icpt)), nil
}

// Correct applies a fitted slope/intercept to a raw tick, rounding
// deterministically to the nearest tick.
func Correct(raw Ticks, slope float64, intercept Ticks) Ticks {
	return Ticks(math.Round(float64(raw)*slope + float64(intercept)))
}
