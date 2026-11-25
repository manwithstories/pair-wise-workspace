package model

import (
	"math/bits"
	"time"
)

// Nanoseconds is the single timestamp representation shared by every adapter.
type Nanoseconds int64

const (
	Nanosecond  Nanoseconds = 1
	Microsecond             = 1000 * Nanosecond
	Millisecond             = 1_000_000 * Nanosecond
	Second                  = 1_000 * Millisecond
)

const maxNanoseconds = 1<<63 - 1

// FromTimecode converts a Matroska-style scaled timecode. timecodeScaleNS is
// the EBML TimecodeScale element (one tick in nanoseconds).
func FromTimecode(value, timecodeScaleNS uint64) Nanoseconds {
	hi, lo := bits.Mul64(value, timecodeScaleNS)
	if hi != 0 || lo > maxNanoseconds {
		return Nanoseconds(maxNanoseconds)
	}
	return Nanoseconds(lo)
}

// FromDuration converts a standard library duration.
func FromDuration(d time.Duration) Nanoseconds { return Nanoseconds(d) }

// Duration returns the timestamp as time.Duration.
func (n Nanoseconds) Duration() time.Duration { return time.Duration(n) }

// Milliseconds returns the timestamp in integer milliseconds.
func (n Nanoseconds) Milliseconds() int64 { return int64(n / Millisecond) }

// RTPTimestamp maps the nanosecond clock to an RTP clock rate. The caller is
// responsible for adding the random RTP offset and wrapping uint32 naturally.
func (n Nanoseconds) RTPTimestamp(clockRate uint32) uint32 {
	if clockRate == 0 {
		return 0
	}
	seconds := uint64(n) / uint64(Second)
	rem := uint64(n) % uint64(Second)
	return uint32(seconds*uint64(clockRate) + rem*uint64(clockRate)/uint64(Second))
}
