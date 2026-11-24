// Package clock owns the shared media-time basis. Capture, reorder, PCM and
// DSP all refer to the same unwrapped timestamps and monotonic output position.
package clock

import (
	"fmt"
	"time"

	"github.com/pion/rtcp"
)

// Rates supported by the capture and processing pipeline.
const (
	Rate44100 = 44100
	Rate48000 = 48000
)

// Mapping is one RTCP Sender Report clock correspondence.
type Mapping struct {
	NTPTime   uint64
	RTPTime   uint32
	MediaTime int64 // unwrapped RTP timestamp in clock-rate units
	ArrivalNS int64
	UnixNano  int64
}

// Base is a stateful, deterministic media clock.
type Base struct {
	ClockRate int

	haveRTP bool
	lastRTP uint32
	media   int64

	// outputPos is the only timeline used by emitted audio. It is advanced
	// monotonically and can never move backwards.
	outputPos int64

	Mappings []Mapping
}

func New(clockRate int) (*Base, error) {
	if clockRate != Rate44100 && clockRate != Rate48000 {
		return nil, fmt.Errorf("unsupported clock rate %d", clockRate)
	}
	return &Base{ClockRate: clockRate}, nil
}

// Unwrap converts a wrapping 32-bit RTP timestamp into a signed-extended value.
// Signed modular differences correctly handle both wrap and moderately reordered
// packets.
func (b *Base) Unwrap(ts uint32) int64 {
	if !b.haveRTP {
		b.haveRTP = true
		b.lastRTP = ts
		b.media = int64(ts)
		return b.media
	}
	delta := int32(ts - b.lastRTP)
	b.media += int64(delta)
	b.lastRTP = ts
	return b.media
}

// Observe returns the unwrapped timestamp without permanently treating a
// late/early packet as the newest clock anchor. It is useful for validation.
func (b *Base) Observe(ts uint32) int64 {
	if !b.haveRTP {
		return int64(ts)
	}
	return b.media + int64(int32(ts-b.lastRTP))
}

// OutputPosition returns the next unwritten output frame.
func (b *Base) OutputPosition() int64 { return b.outputPos }

// AdvanceOutput records n emitted source-rate frames. It rejects any attempt to
// seek the output timeline backwards.
func (b *Base) AdvanceOutput(expected, n int64) (int64, error) {
	if expected != b.outputPos {
		return b.outputPos, fmt.Errorf("media time went backwards: expected %d, got %d", b.outputPos, expected)
	}
	if n < 0 {
		return b.outputPos, fmt.Errorf("negative duration %d", n)
	}
	b.outputPos += n
	return b.outputPos, nil
}

// ApplySenderReport records the RTP-to-wall-clock mapping from an RTCP SR.
func (b *Base) ApplySenderReport(sr *rtcp.SenderReport, arrivalNS int64) Mapping {
	m := Mapping{
		NTPTime:   sr.NTPTime,
		RTPTime:   sr.RTPTime,
		MediaTime: b.Observe(sr.RTPTime),
		ArrivalNS: arrivalNS,
		UnixNano:  NTPToUnixNano(sr.NTPTime),
	}
	b.Mappings = append(b.Mappings, m)
	return m
}

// WallTime estimates the wall-clock time of an RTP timestamp from the newest
// Sender Report. The result is deterministic for a fixed input.
func (b *Base) WallTime(rtpTS uint32) (time.Time, bool) {
	if len(b.Mappings) == 0 {
		return time.Time{}, false
	}
	m := b.Mappings[len(b.Mappings)-1]
	target := b.Observe(rtpTS)
	delta := target - m.MediaTime
	ns := m.UnixNano + delta*int64(time.Second)/int64(b.ClockRate)
	return time.Unix(0, ns).UTC(), true
}

// NTPToUnixNano converts the 64-bit NTP timestamp used in RTCP SR to Unix ns.
func NTPToUnixNano(ntp uint64) int64 {
	const ntpEpochOffsetSeconds = 2208988800
	seconds := int64(ntp>>32) - ntpEpochOffsetSeconds
	fraction := uint64(ntp & 0xffffffff)
	nanos := int64((fraction * 1_000_000_000) >> 32)
	return seconds*int64(time.Second) + nanos
}
