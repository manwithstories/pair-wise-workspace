// Package timeline unwraps MPEG timestamps and estimates RTP clock drift.
package timeline

import (
	"math"
	"sync"

	"mediad/internal/mediaerr"
)

const (
	PTSHz     = 90_000
	PCRBaseHz = 90_000
	PCRHz     = 27_000_000
	mod33     = int64(1) << 33
	half33    = mod33 / 2
	mod32     = int64(1) << 32
	half32    = mod32 / 2
	maxWindow = 64
	outlierNS = int64(250_000_000)
)

// Unwrapper reconstructs a monotonically increasing value from fixed-width
// wraparound samples using shortest signed modular differences.
type Unwrapper struct {
	last int64
	have bool
}

// Unwrap33 unwraps PTS/DTS and PCR_base values.
func (u *Unwrapper) Unwrap33(raw uint64) (int64, error) {
	if raw >= uint64(mod33) {
		return 0, mediaerr.New(mediaerr.KindTimestampOverflow, -1, 0, true,
			"33-bit timestamp overflow: %d", raw)
	}
	return u.unwrap(int64(raw), mod33, half33), nil
}

// Unwrap32 unwraps RTP timestamps.
func (u *Unwrapper) Unwrap32(raw uint32) int64 {
	return u.unwrap(int64(raw), mod32, half32)
}

func (u *Unwrapper) unwrap(raw, mod, half int64) int64 {
	if !u.have {
		u.have = true
		u.last = raw
		return raw
	}
	delta := raw - (u.last % mod)
	if delta > half {
		delta -= mod
	} else if delta < -half {
		delta += mod
	}
	u.last += delta
	return u.last
}

// PCR is an unwrapped PCR in 27 MHz ticks and 90 kHz base ticks.
type PCR struct {
	Ticks27MHz int64
	Base90kHz  int64
	Extension  uint16
}

// UnwrapPCR unwraps the 33-bit base and combines base/extension.
func UnwrapPCR(u *Unwrapper, base uint64, ext uint16) (PCR, error) {
	if ext > 299 {
		return PCR{}, mediaerr.New(mediaerr.KindTimestampOverflow, -1, 0, true,
			"PCR extension %d exceeds 299", ext)
	}
	b, err := u.Unwrap33(base)
	if err != nil {
		return PCR{}, err
	}
	return PCR{Base90kHz: b, Ticks27MHz: b*300 + int64(ext), Extension: ext}, nil
}

// ArrivalSample is one bounded RTP timestamp/arrival observation.
type ArrivalSample struct {
	RTPTicks  int64
	ArrivalNS int64
	Accepted  bool
}

// Drift is the robust clock estimate from arrival observations.
type Drift struct {
	DriftPPM       float64
	SlopeNSPerTick float64
	Accepted       int
	Outliers       int
}

// Clock keeps one common media time base and a bounded drift window. Outliers
// are counted but never replace the anchor or reset unwrapped timestamps.
type Clock struct {
	mu         sync.Mutex
	rtp        Unwrapper
	haveAnchor bool
	anchorRTP  int64
	anchorNS   int64
	samples    []ArrivalSample
	outliers   int
	rate       uint32
}

// NewClock creates a clock for an RTP clock rate (usually 90 kHz).
func NewClock(rtpClockRate uint32) *Clock {
	if rtpClockRate == 0 {
		rtpClockRate = PTSHz
	}
	return &Clock{rate: rtpClockRate}
}

// Observe records an RTP timestamp and local arrival time in nanoseconds.
func (c *Clock) Observe(rtpTimestamp uint32, arrivalNS int64) Drift {
	c.mu.Lock()
	defer c.mu.Unlock()

	media := c.rtp.Unwrap32(rtpTimestamp)
	if !c.haveAnchor {
		c.haveAnchor = true
		c.anchorRTP = media
		c.anchorNS = arrivalNS
		c.samples = append(c.samples, ArrivalSample{RTPTicks: media, ArrivalNS: arrivalNS, Accepted: true})
		return c.estimateLocked()
	}

	nominalNS := float64(media-c.anchorRTP) * 1e9 / float64(c.rate)
	predicted := c.anchorNS + int64(nominalNS)
	residual := arrivalNS - predicted
	if math.Abs(float64(residual)) > float64(outlierNS) && len(c.samples) >= 2 {
		c.outliers++
		return c.estimateLocked()
	}

	c.samples = append(c.samples, ArrivalSample{RTPTicks: media, ArrivalNS: arrivalNS, Accepted: true})
	if len(c.samples) > maxWindow {
		c.samples = c.samples[len(c.samples)-maxWindow:]
	}
	return c.estimateLocked()
}

// Estimate returns the current drift estimate.
func (c *Clock) Estimate() Drift {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.estimateLocked()
}

func (c *Clock) estimateLocked() Drift {
	n := len(c.samples)
	d := Drift{Outliers: c.outliers, Accepted: n}
	if n < 2 {
		return d
	}
	var mx, my float64
	for _, s := range c.samples {
		x := float64(s.RTPTicks - c.anchorRTP)
		y := float64(s.ArrivalNS - c.anchorNS)
		mx += x
		my += y
	}
	mx /= float64(n)
	my /= float64(n)
	var num, den float64
	for _, s := range c.samples {
		x := float64(s.RTPTicks-c.anchorRTP) - mx
		y := float64(s.ArrivalNS-c.anchorNS) - my
		num += x * y
		den += x * x
	}
	if den == 0 {
		return d
	}
	slope := num / den // arrival ns per RTP tick
	nominal := 1e9 / float64(c.rate)
	d.SlopeNSPerTick = slope
	if slope > 0 {
		d.DriftPPM = (nominal/slope - 1) * 1e6
	}
	return d
}

// PTSDurationMS converts a 90 kHz tick difference to milliseconds.
func PTSDurationMS(ticks int64) float64 {
	return float64(ticks) * 1000 / PTSHz
}

// BoundarySkewMS returns absolute skew between two rendition cut PTS values.
func BoundarySkewMS(a, b int64) float64 {
	return math.Abs(PTSDurationMS(a - b))
}
