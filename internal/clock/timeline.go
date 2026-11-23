// Package clock unifies RTP and container time bases onto one monotonic
// microsecond timeline, including 32-bit RTP timestamp wraparound.
package clock

import (
	"math"

	"github.com/example/mediad/internal/model"
)

// Micros is the unified time unit used across every pipeline stage.
// Values are signed so that offsets produced while aligning tracks stay
// representable.
type Micros int64

const (
	MicrosPerSecond = 1_000_000
	// rtpModulo is the 32-bit RTP timestamp modulus.
	rtpModulo int64 = 1 << 32
	// wrapThreshold: a negative jump smaller than this counts as wraparound
	// rather than a reordering anomaly.
	wrapThreshold int64 = rtpModulo / 2
)

// RTPClock converts a single RTP stream's clock-rate timestamps to micros.
type RTPClock struct {
	ClockRate uint32
	// anchorRTP is the raw 32-bit timestamp at the anchor point.
	anchorRTP uint32
	// anchorMicros is the unified-time value the anchor maps to.
	anchorMicros Micros
	// unwrapped is the running unwrapped 64-bit timestamp.
	unwrapped int64
	started   bool
}

// NewRTPClock creates a clock. anchorMicros is usually zero for the first
// packet of a stream.
func NewRTPClock(clockRate uint32, anchorMicros Micros) *RTPClock {
	if clockRate == 0 {
		clockRate = 90000
	}
	return &RTPClock{ClockRate: clockRate, anchorMicros: anchorMicros}
}

// Unwrap maps a raw 32-bit RTP timestamp to a monotonic 64-bit timestamp
// on the stream's own clock. The first value seen becomes the origin.
func (c *RTPClock) Unwrap(ts uint32) int64 {
	raw := int64(ts)
	if !c.started {
		c.started = true
		c.anchorRTP = ts
		c.unwrapped = raw
		return raw
	}
	delta := raw - (c.unwrapped % rtpModulo)
	if delta < -wrapThreshold {
		delta += rtpModulo
	} else if delta > wrapThreshold {
		delta -= rtpModulo
	}
	c.unwrapped += delta
	return c.unwrapped
}

// ToMicros converts an unwrapped RTP timestamp to unified micros.
func (c *RTPClock) ToMicros(unwrappedTS int64) Micros {
	rel := unwrappedTS - int64(c.anchorRTP)
	return c.anchorMicros + Micros(math.Round(float64(rel)*MicrosPerSecond/float64(c.ClockRate)))
}

// Convert is Unwrap followed by ToMicros in one call.
func (c *RTPClock) Convert(ts uint32) Micros {
	return c.ToMicros(c.Unwrap(ts))
}

// ContainerClock maps container timescale units (e.g. MP4 mdhd timescale,
// MXF edit-unit durations) to unified micros.
type ContainerClock struct {
	Timescale uint32
}

// NewContainerClock rejects a zero timescale.
func NewContainerClock(timescale uint32) (*ContainerClock, error) {
	if timescale == 0 {
		return nil, model.New(model.KindCorrupt, "clock", "container-clock",
			"container declares zero timescale")
	}
	return &ContainerClock{Timescale: timescale}, nil
}

// ToMicros converts a container-time delta to micros.
func (c *ContainerClock) ToMicros(units int64) Micros {
	return Micros(math.Round(float64(units) * MicrosPerSecond / float64(c.Timescale)))
}

// Timeline anchors several independent clocks to a shared origin so that
// RTP streams and container tracks can be compared directly.
type Timeline struct {
	origin Micros
	clocks map[string]*RTPClock
}

// NewTimeline creates a timeline whose origin is the given micros value.
func NewTimeline(origin Micros) *Timeline {
	return &Timeline{origin: origin, clocks: map[string]*RTPClock{}}
}

// AddRTP registers an RTP stream; its first observed timestamp is pinned
// to streamOffsetMicros relative to the timeline origin.
func (t *Timeline) AddRTP(streamID string, clockRate uint32, streamOffset Micros) *RTPClock {
	c := NewRTPClock(clockRate, t.origin+streamOffset)
	t.clocks[streamID] = c
	return c
}

// RTP returns a previously registered RTP clock.
func (t *Timeline) RTP(streamID string) (*RTPClock, bool) {
	c, ok := t.clocks[streamID]
	return c, ok
}

// Origin returns the timeline origin.
func (t *Timeline) Origin() Micros { return t.origin }

// OffsetMillis is a convenience for A/V offset reporting.
func OffsetMillis(a, b Micros) int64 {
	return int64((a - b) / 1000)
}
