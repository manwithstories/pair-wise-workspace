// Package timeline defines the shared time base and error model used by every
// pipeline stage. RTP timestamps run on a 90 kHz media clock and wrap at 2^32;
// RTCP sender reports anchor that clock to wall-clock (NTP) time.
package timeline

// RTPClockRate is the RFC 2250 MPEG-TS video clock rate in ticks per second.
const RTPClockRate = 90_000

// WrapMod is the modulus of the 32-bit RTP timestamp / sequence counter.
const WrapMod uint64 = 1 << 32

// MaxSaneJump bounds how far a single timestamp may move before it is treated
// as a clock jump rather than normal progress. Five media seconds is far
// larger than any inter-packet spacing at 25 fps.
const MaxSaneJump uint64 = 5 * RTPClockRate
