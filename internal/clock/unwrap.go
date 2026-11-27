// Package clock unwraps 33-bit PCR/PTS timestamps onto the shared
// 27 MHz base and estimates bounded clock drift.
package clock

import "tscheck/internal/model"

// halfRange33 is half of the 33-bit timestamp modulus, the maximum
// forward step between two consecutive timestamps.
const halfRange33 int64 = 1 << 32

// Clock maintains the unwrap state shared by PCR and PTS. PCR values are
// the trusted anchors; a PTS observed before any PCR belongs to an
// unrecoverable epoch.
type Clock struct {
	pcrInit     bool
	pcrEpoch    int
	lastPCRRaw  int64 // last raw 33-bit PCR base (90 kHz)
	lastPCRTick model.Ticks
	wraps       int
}

// NewClock creates a Clock with no anchors.
func NewClock() *Clock { return &Clock{} }

// Wraps reports how many PCR wraps have been observed.
func (c *Clock) Wraps() int { return c.wraps }

// Anchored reports whether at least one trusted PCR has been seen.
func (c *Clock) Anchored() bool { return c.pcrInit }

// ObservePCR registers a PCR (33-bit 90 kHz base + 9-bit 27 MHz
// extension) and returns its unwrapped position on the shared base.
func (c *Clock) ObservePCR(base, ext int64) model.Timestamp {
	if c.pcrInit && base < c.lastPCRRaw-halfRange33 {
		c.pcrEpoch++
		c.wraps++
	}
	c.lastPCRRaw = base
	c.pcrInit = true
	unwrapped := base + int64(c.pcrEpoch)<<33
	c.lastPCRTick = model.RawPCRToTicks(unwrapped, ext)
	return model.Timestamp{Ticks: c.lastPCRTick, Epoch: c.pcrEpoch, Anchored: true}
}

// UnwrapPTS maps a raw 33-bit 90 kHz PTS onto the shared base, anchored
// to the current PCR epoch. Without a PCR anchor the epoch is marked
// unrecoverable (Anchored=false, Epoch=-1).
func (c *Clock) UnwrapPTS(raw int64) model.Timestamp {
	if !c.pcrInit {
		return model.Timestamp{Ticks: model.PTS90kToTicks(raw), Epoch: -1, Anchored: false}
	}
	v := raw + int64(c.pcrEpoch)<<33
	pcrBase := c.lastPCRRaw + int64(c.pcrEpoch)<<33
	for v-pcrBase > halfRange33 {
		v -= 1 << 33
	}
	for pcrBase-v > halfRange33 {
		v += 1 << 33
	}
	return model.Timestamp{Ticks: model.PTS90kToTicks(v), Epoch: c.pcrEpoch, Anchored: true}
}
