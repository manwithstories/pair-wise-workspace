// Package model defines the shared 27 MHz time base and timestamp
// primitives used across the demux, clock, analysis and fanout layers.
package model

// ClockHz is the common MPEG-TS system time base: 27 MHz.
const ClockHz int64 = 27_000_000

// PTSHz is the 90 kHz base used by PTS/DTS and the PCR base field.
const PTSHz int64 = 90_000

// Raw33 is a raw 33-bit timestamp value (PCR base or PTS/DTS).
type Raw33 int64

// MaxRaw33 is the modulus of the 33-bit timestamp domain.
const MaxRaw33 Raw33 = 1 << 33

// Ticks is a media time expressed on the common 27 MHz base.
// Values are epoch-extended (unwrapped), so they are monotonic within a
// trusted epoch and never wrap.
type Ticks int64

// PTS90kToTicks converts a 90 kHz timestamp to 27 MHz ticks.
func PTS90kToTicks(v int64) Ticks { return Ticks(v * (ClockHz / PTSHz)) }

// RawPCRToTicks converts a PCR (33-bit 90 kHz base + 9-bit 27 MHz
// extension) to 27 MHz ticks.
func RawPCRToTicks(base, ext int64) Ticks {
	return Ticks(base*(ClockHz/PTSHz) + ext)
}

// Millis converts ticks to milliseconds.
func (t Ticks) Millis() float64 { return float64(t) * 1000.0 / float64(ClockHz) }

// Timestamp is a media time together with its trust provenance.
type Timestamp struct {
	Ticks Ticks
	// Epoch identifies the unwrap epoch the value belongs to.
	Epoch int
	// Anchored reports whether the epoch was anchored to a trusted PCR.
	// Unanchored epochs are unrecoverable: their absolute position on the
	// shared clock is unknown.
	Anchored bool
}

// FrameTicks is the nominal duration of one frame in 27 MHz ticks used by
// the boundary merge: deltas beyond one frame are conflicts.
const FrameTicks = Ticks(ClockHz / 30)
