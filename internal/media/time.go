// Package media defines the shared media clock and fault model used by every
// avlab module. All timestamps on the timeline are expressed in ticks of a
// single 90 kHz clock, regardless of the underlying codec sample rate.
package media

import (
	"errors"
	"math"
	"time"
)

// ClockRate is the unified RTP/MPEG-TS media clock rate in hertz.
const ClockRate uint32 = 90_000

// MaxTick is the largest representable media timestamp.
const MaxTick MediaTime = math.MaxInt64

// MediaTime is a point on the 90 kHz media timeline measured in ticks.
type MediaTime int64

// Epoch identifies a continuous timeline segment. An HLS discontinuity or a
// DASH representation switch at a segment boundary opens a new epoch.
type Epoch uint32

// Stamp is a timestamp qualified by its epoch. Late packets belonging to an
// older epoch can therefore be rejected instead of polluting a new timeline.
type Stamp struct {
	Epoch Epoch
	Tick  MediaTime
}

// FromDuration converts a wall-clock duration to 90 kHz ticks, rounding to
// the nearest tick.
func FromDuration(d time.Duration) MediaTime {
	return MediaTime(d * time.Duration(ClockRate) / time.Second)
}

// Duration converts ticks back to a wall-clock duration.
func (t MediaTime) Duration() time.Duration {
	return time.Duration(t) * time.Second / time.Duration(ClockRate)
}

// Milliseconds returns the timestamp expressed in milliseconds as a float.
func (t MediaTime) Milliseconds() float64 {
	return float64(t) * 1000 / float64(ClockRate)
}

// Add shifts the timestamp by d ticks. It returns ErrTickOverflow when the
// result leaves the representable range.
func (t MediaTime) Add(d MediaTime) (MediaTime, error) {
	r := int64(t) + int64(d)
	if (d > 0 && r < int64(t)) || (d < 0 && r > int64(t)) {
		return 0, ErrTickOverflow
	}
	return MediaTime(r), nil
}

// Sub returns t - o in ticks.
func (t MediaTime) Sub(o MediaTime) MediaTime { return t - o }

// Before reports whether t is strictly earlier than o.
func (t MediaTime) Before(o MediaTime) bool { return t < o }

// After reports whether t is strictly later than o.
func (t MediaTime) After(o MediaTime) bool { return t > o }

// CheckedAdd is the package-level overflow-checked addition helper.
func CheckedAdd(base, delta MediaTime) (MediaTime, error) { return base.Add(delta) }

// ErrTickOverflow is returned when a timestamp computation overflows.
var ErrTickOverflow = errors.New("media: timestamp overflow")
