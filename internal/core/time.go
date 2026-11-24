// Package core contains the unified media clock and classified errors used by
// every mediq stage.
package core

import (
	"fmt"
	"math"
	"time"
)

const (
	// OutputRateHz is the fixed delivery sample rate.
	OutputRateHz = 48_000
	// OutputChannels is the fixed delivery channel count.
	OutputChannels = 2
	// OutputBytesPerSample is signed 16-bit LPCM.
	OutputBytesPerSample = 2
	// TargetBPS is 48 kHz * stereo * 16 bits.
	TargetBPS = OutputRateHz * OutputChannels * OutputBytesPerSample * 8
)

// Time is an output-rate sample position. The zero value is the stream origin.
type Time int64

// Samples returns the position as an integer sample count.
func (t Time) Samples() int64 { return int64(t) }

// Duration converts the position to a time.Duration.
func (t Time) Duration() time.Duration {
	return time.Duration(float64(t) / float64(OutputRateHz) * float64(time.Second))
}

// Milliseconds returns fractional milliseconds, suitable for manifest checks.
func (t Time) Milliseconds() float64 {
	return float64(t) * 1000 / OutputRateHz
}

// Clock is a traceable RTP-to-unified-time model.
//
// RTP timestamps are expanded to 64 bits before mapping. Until a Sender Report
// is observed, Map uses the nominal source rate and the first timestamp as the
// origin. SR observations estimate linear clock drift and provide an NTP
// anchor.
type Clock struct {
	SourceRateHz uint32
	SSRC         uint32

	originSet   bool
	originRTP   int64
	lastExtended int64
	hasLast     bool

	hasSR       bool
	srRTP       int64
	srNTP       float64
	baseRTP     int64
	baseNTP     float64
	driftPPM    float64
	observations int
}

// NewClock creates a clock for an RTP timestamp frequency.
func NewClock(ssrc, sourceRateHz uint32) *Clock {
	if sourceRateHz == 0 {
		sourceRateHz = OutputRateHz
	}
	return &Clock{SSRC: ssrc, SourceRateHz: sourceRateHz}
}

// ExtendTimestamp expands a 32-bit RTP timestamp around the latest value.
func (c *Clock) ExtendTimestamp(ts uint32) int64 {
	u := int64(ts)
	if !c.hasLast {
		c.lastExtended = u
		c.hasLast = true
		if !c.originSet {
			c.originRTP = u
			c.originSet = true
		}
		return u
	}

	const wrap = 1 << 32
	prev := c.lastExtended
	candidate := u + wrap*int64(math.Round(float64(prev-u)/wrap))
	// A second forward wrap can occur after long silence or reordering.
	for candidate-prev > wrap/2 {
		candidate -= wrap
	}
	for prev-candidate > wrap/2 {
		candidate += wrap
	}
	c.lastExtended = candidate
	return candidate
}

// ObserveSenderReport records an SR mapping. ntpSeconds is the NTP epoch
// seconds field and ntpFraction is the fixed-point fractional field.
func (c *Clock) ObserveSenderReport(rtpTS uint32, ntpSeconds uint32, ntpFraction uint32) {
	rtp := c.ExtendTimestamp(rtpTS)
	ntp := NTPToFloat(ntpSeconds, ntpFraction)
	if !c.hasSR {
		c.hasSR = true
		c.srRTP, c.srNTP = rtp, ntp
		c.baseRTP, c.baseNTP = rtp, ntp
		c.observations = 1
		return
	}

	dtNTP := ntp - c.baseNTP
	dRTP := float64(rtp - c.baseRTP)
	if dtNTP > 0 && dRTP >= 0 {
		nominal := float64(c.SourceRateHz) * dtNTP
		if nominal > 0 {
			c.driftPPM = (dRTP - nominal) / nominal * 1_000_000
		}
	}
	c.srRTP, c.srNTP = rtp, ntp
	c.observations++
}

// DriftPPM returns the latest estimated RTP clock drift.
func (c *Clock) DriftPPM() float64 { return c.driftPPM }

// HasSenderReport reports whether an SR corrected the clock.
func (c *Clock) HasSenderReport() bool { return c.hasSR }

// Map converts an RTP timestamp to unified 48 kHz sample position.
func (c *Clock) Map(ts uint32) Time {
	rtp := c.ExtendTimestamp(ts)
	if !c.originSet {
		c.originRTP = rtp
		c.originSet = true
	}

	var sourceSeconds float64
	if c.hasSR {
		// Anchor at the most recent SR. Extrapolate by nominal rate and remove
		// the estimated linear drift from the RTP device's elapsed duration.
		rtpElapsed := float64(rtp - c.srRTP)
		effectiveRate := float64(c.SourceRateHz) * (1 + c.driftPPM/1_000_000)
		if effectiveRate <= 0 || math.Abs(c.driftPPM) > 100_000 {
			effectiveRate = float64(c.SourceRateHz)
		}
		sourceSeconds = c.srNTP - c.baseNTP + rtpElapsed/effectiveRate
		// Make the first mapped event the zero origin while retaining the NTP
		// relation in diagnostics.
		originSeconds := float64(c.originRTP-c.baseRTP) / effectiveRate
		sourceSeconds -= originSeconds
	} else {
		sourceSeconds = float64(rtp-c.originRTP) / float64(c.SourceRateHz)
	}
	return Time(math.Round(sourceSeconds * OutputRateHz))
}

// MapDuration maps a source sample duration to output samples.
func (c *Clock) MapDuration(sourceSamples int) int64 {
	if c.SourceRateHz == OutputRateHz {
		return int64(sourceSamples)
	}
	return int64(math.Round(float64(sourceSamples) * OutputRateHz / float64(c.SourceRateHz)))
}

// NTPToFloat converts the 64-bit NTP timestamp fields to fractional seconds
// in the NTP epoch.
func NTPToFloat(seconds, fraction uint32) float64 {
	return float64(seconds) + float64(fraction)/(1<<32)
}

// NTPToUnix converts NTP seconds to Unix seconds.
func NTPToUnix(ntpSeconds uint32) time.Time {
	const ntpToUnix = 2_208_988_800
	return time.Unix(int64(ntpSeconds)-ntpToUnix, 0).UTC()
}

// FormatMS renders a millisecond value with three decimal places.
func FormatMS(samples int64) string {
	return fmt.Sprintf("%.3f", Time(samples).Milliseconds())
}
