package clock

import (
	"errors"
	"time"
)

// TickRate is the unified media clock rate used by every track: 90 kHz.
const TickRate uint32 = 90000

// MaxDuration is the per-track duration limit (10 minutes).
const MaxDuration = 10 * time.Minute

// Tick is an instant on the shared 90 kHz media clock.
type Tick int64

// FromTime converts a wall-clock duration relative to track start into a
// Tick value, rounding to the nearest tick.
func FromTime(d time.Duration) Tick {
	return Tick((d*time.Duration(TickRate) + time.Second/2) / time.Second)
}

// ToTime converts a tick offset back to a wall-clock duration.
func (t Tick) ToTime() time.Duration {
	return time.Duration(t) * time.Second / time.Duration(TickRate)
}

// Millis renders the tick offset as milliseconds (float, for metrics).
func (t Tick) Millis() float64 {
	return float64(t) * 1000 / float64(TickRate)
}

// FromSamples maps a sample count at the given sample rate to ticks.
func FromSamples(samples int64, sampleRate uint32) Tick {
	return Tick(samples * int64(TickRate) / int64(sampleRate))
}

// ToSamples maps ticks to a sample count at the given sample rate, rounded
// to the nearest sample so fragment boundaries never lose or repeat samples.
func (t Tick) ToSamples(sampleRate uint32) int64 {
	num := int64(t)*int64(sampleRate)*2 + int64(TickRate)
	return num / (2 * int64(TickRate))
}

// Wrap16 performs modular subtraction of two 16-bit counters and returns
// the signed distance from a to b in the circular sequence space.
func Wrap16(a, b uint16) int {
	d := int32(b) - int32(a)
	if d > 32767 {
		d -= 65536
	}
	if d < -32768 {
		d += 65536
	}
	return int(d)
}

// Wrap32 performs modular subtraction of two 32-bit counters (RTP/RTCP
// timestamps) and returns the signed distance from a to b.
func Wrap32(a, b uint32) int64 {
	d := int64(int64(uint32(b)) - int64(uint32(a)))
	if d > 1<<31 {
		d -= 1 << 32
	}
	if d < -(1 << 31) {
		d += 1 << 32
	}
	return d
}

// Timeline maps a track's RTP timestamp space onto the shared 90 kHz tick
// space. The mapping is affine: tick = rtp*scale + offset, where scale
// converts the track clock rate to 90 kHz.
type Timeline struct {
	rate   uint32 // RTP clock rate of the track
	scale  float64
	valid  bool
	offset Tick // tick - scaledRtp, once calibrated
	// last calibration inputs, used to detect report jumps
	lastRTP   uint32
	lastNTP   Tick
	haveLast  bool
}

// NewTimeline creates a timeline for a track running at rtpClockRate.
func NewTimeline(rtpClockRate uint32) *Timeline {
	if rtpClockRate == 0 {
		rtpClockRate = TickRate
	}
	return &Timeline{rate: rtpClockRate, scale: float64(TickRate) / float64(rtpClockRate)}
}

// Calibrate records an RTCP Sender Report pairing: RTP timestamp rtp at
// wall-clock tick ntp (already expressed on the 90 kHz reference clock).
// A report whose pairing jumps implausibly relative to the previous report
// is rejected and the last valid mapping is retained.
func (tl *Timeline) Calibrate(rtp uint32, ntp Tick) error {
	if tl.haveLast {
		dRTP := Wrap32(tl.lastRTP, rtp)
		dNTP := int64(ntp) - int64(tl.lastNTP)
		// Expected tick delta implied by the RTP delta.
		want := int64(float64(dRTP) * tl.scale)
		jump := dNTP - want
		if jump < 0 {
			jump = -jump
		}
		// More than 250 ms of unexplained jump is treated as a bad report.
		if jump > int64(TickRate)/4 {
			return NewError(StageRTCP, ErrReportJump, 0, true,
				"sender report jump exceeds tolerance; retaining last mapping")
		}
	}
	tl.offset = ntp - Tick(float64(rtp)*tl.scale)
	tl.lastRTP, tl.lastNTP, tl.haveLast = rtp, ntp, true
	tl.valid = true
	return nil
}

// Valid reports whether at least one accepted Sender Report exists.
func (tl *Timeline) Valid() bool { return tl.valid }

// ToTick maps an RTP timestamp to the shared clock. Before the first valid
// report the mapping is anchored at RTP 0 -> tick 0 and ErrNoReport is
// surfaced so callers can record the degraded condition.
func (tl *Timeline) ToTick(rtp uint32) (Tick, error) {
	t := Tick(float64(rtp)*tl.scale) + tl.offset
	if !tl.valid {
		return t, NewError(StageRTCP, ErrNoReport, 0, true,
			"no sender report yet; using provisional zero-anchored mapping")
	}
	return t, nil
}

// ErrLimit is returned when a configured limit is exceeded.
var ErrLimit = errors.New("configured limit exceeded")

// CheckDuration enforces the per-track 10 minute cap.
func CheckDuration(d time.Duration) error {
	if d > MaxDuration {
		return NewError(StageMetrics, ErrDurationTooLong, 0, false,
			"track duration "+d.String()+" exceeds "+MaxDuration.String())
	}
	return nil
}
