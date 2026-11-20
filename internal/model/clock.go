// Package model defines the time base and fault vocabulary shared by every
// mediamend module. All timestamps inside the pipeline are normalized to the
// same 90kHz master clock (the RTP video clock rate), regardless of the clock
// of the capturing device.
package model

import "fmt"

// MasterRate is the ticks-per-second rate of the unified timeline. 90kHz is
// the canonical RTP video clock and divides every common media rate
// (8/16/24/32/44.1/48kHz audio, 23.976/24/25/29.97/30/50/60 fps video).
const MasterRate uint64 = 90000

// RTPMod is the 32-bit RTP timestamp rollover modulus.
const RTPMod uint64 = 1 << 32

// Clock describes how a device stamps its frames and how those stamps map to
// the unified timeline.
type Clock struct {
	// TrackID is the track this clock belongs to.
	TrackID string
	// Rate is the device timestamp ticks-per-second (e.g. 90000 video,
	// 48000 audio).
	Rate uint64
	// AnchorSeq is the sequence number whose DeviceTS is trusted.
	AnchorSeq uint64
	// AnchorDeviceTS is the raw device timestamp at AnchorSeq.
	AnchorDeviceTS uint64
	// AnchorMasterTS is the master-clock timestamp at AnchorSeq, fixed when
	// the project timeline is established.
	AnchorMasterTS uint64
	// FrameStep is the device-clock distance between two consecutive frames
	// (e.g. 3000 ticks at 90kHz for 30fps, 48 ticks at 48kHz for 1ms audio).
	FrameStep uint64
}

// ToMaster converts a raw device tick count into master-clock ticks using the
// clock rate. Conversion is rounded to the nearest master tick.
func (c Clock) ToMaster(deviceTS uint64) uint64 {
	return deviceTS * MasterRate / c.Rate
}

// ExpectedMaster returns the master timestamp the frame carrying seq should
// have given the anchor and the per-frame device-clock step.
func (c Clock) ExpectedMaster(seq uint64) uint64 {
	delta := seq - c.AnchorSeq
	return c.AnchorMasterTS + c.ToMaster(delta*c.FrameStep)
}

// Unwrap resolves a 32-bit RTP timestamp that may have rolled over relative
// to the previous (already unwrapped) value. A jump backwards of more than
// half the modulus is treated as exactly one forward rollover. The returned
// value is always >= prev unless the input is a genuine backwards step
// smaller than half the modulus (a device-clock reset is handled elsewhere).
func Unwrap(prev, raw uint64) uint64 {
	base := prev - prev%RTPMod
	cand := base + raw
	switch {
	case cand > prev:
		// Large forward jump means raw is actually from the previous cycle.
		// The guard keeps early readings near the modulus on a fresh clock:
		// unwrapped time must never go negative.
		if cand-prev > RTPMod/2 && cand >= RTPMod {
			cand -= RTPMod
		}
	case prev > cand:
		// Backwards step: a small one is genuine clock drift; a large one is
		// a forward rollover. Signed-free comparisons avoid uint underflow.
		if prev-cand >= RTPMod/2 {
			cand += RTPMod
		}
	}
	return cand
}

// IsRollover reports whether raw, seen after the unwrapped timestamp prev, is
// a forward 32-bit rollover: unwrapping it must advance the clock while the
// raw 32-bit value is smaller than the low word of the previous value.
func IsRollover(prev, raw uint64) bool {
	unwrapped := Unwrap(prev, raw)
	return unwrapped > prev && raw < prev%RTPMod
}

// NanoToMaster converts a duration expressed in nanoseconds to master ticks.
func NanoToMaster(nanos int64) int64 {
	return nanos * int64(MasterRate) / 1_000_000_000
}

// MasterToNano converts master ticks to a nanosecond duration.
func MasterToNano(ticks uint64) uint64 {
	return ticks * 1_000_000_000 / MasterRate
}

// String renders the clock for manifests and logs.
func (c Clock) String() string {
	return fmt.Sprintf("Clock(track=%s rate=%d anchorSeq=%d anchorDev=%d anchorMaster=%d)",
		c.TrackID, c.Rate, c.AnchorSeq, c.AnchorDeviceTS, c.AnchorMasterTS)
}
