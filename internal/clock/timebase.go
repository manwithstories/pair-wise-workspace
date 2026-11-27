// Package clock defines the shared 90 kHz media timebase used by every stage
// of the relay audit pipeline: record parsing, frame inspection, timeline
// resolution and repair.
//
// Broadcast RLY1 records carry a 32-bit PTS that wraps roughly every 13.2
// hours at 90 kHz. Extender maps those raw values onto a monotonically
// increasing 64-bit timeline so that keyframe intervals and clip boundaries
// remain correct across a wrap.
package clock

import "time"

const (
	// Rate is the shared media clock rate in ticks per second.
	Rate uint64 = 90000
	// Wrap is the size of the 32-bit PTS space.
	Wrap = uint64(1) << 32
	// HalfWrap is the distance at which a raw stamp is assumed to have
	// wrapped rather than moved backwards.
	HalfWrap = Wrap / 2

	wrapMask = Wrap - 1
)

// Stamp is a raw media timestamp in 90 kHz ticks as stored on the wire.
type Stamp uint32

// Ticks returns the raw stamp widened to 64 bits.
func (s Stamp) Ticks() uint64 { return uint64(s) }

// Extend maps raw onto the extension of prev that is closest to prev. The
// second result reports whether a wraparound was crossed. prev must be the
// most recent extended value; the initial value may be any raw stamp.
func Extend(prev uint64, raw Stamp) (uint64, bool) {
	cand := (prev &^ wrapMask) | uint64(raw)
	if cand+HalfWrap < prev {
		return cand + Wrap, true
	}
	return cand, false
}

// Elapsed returns the forward distance from start to cur in the 32-bit PTS
// space. It is used for stateless 32-bit mapping where the whole session
// spans fewer than 2^32 ticks.
func Elapsed(start, cur Stamp) uint64 {
	return uint64(uint32(cur - start))
}

// Extender turns a sequence of raw PTS values into ticks relative to the
// first value observed. The first value maps to zero.
type Extender struct {
	origin uint64
	last   uint64
	has    bool
	wraps  int
}

// Next extends raw and returns its offset from the first observed stamp.
func (e *Extender) Next(raw Stamp) (ticks uint64, wrapped bool) {
	if !e.has {
		e.origin = uint64(raw)
		e.last = e.origin
		e.has = true
		return 0, false
	}
	abs, wrapped := Extend(e.last, raw)
	if wrapped {
		e.wraps++
	}
	e.last = abs
	return abs - e.origin, wrapped
}

// Started reports whether at least one stamp has been observed.
func (e *Extender) Started() bool { return e.has }

// Last returns the most recent extended tick relative to the origin.
func (e *Extender) Last() uint64 {
	if !e.has {
		return 0
	}
	return e.last - e.origin
}

// Origin returns the absolute extended value of the first observed stamp.
func (e *Extender) Origin() uint64 { return e.origin }

// Wraps returns the number of wraparounds observed so far.
func (e *Extender) Wraps() int { return e.wraps }

// Restore reinstates a checkpointed extender position.
func (e *Extender) Restore(origin, last uint64, wraps int) {
	e.origin = origin
	e.last = origin + last
	e.wraps = wraps
	e.has = true
}

// Ticks converts a wall-clock duration to 90 kHz ticks.
func Ticks(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d) * Rate / uint64(time.Second)
}

// Duration converts 90 kHz ticks to a wall-clock duration.
func Duration(ticks uint64) time.Duration {
	return time.Duration(ticks) * time.Second / time.Duration(Rate)
}

// Millis converts 90 kHz ticks to whole milliseconds.
func Millis(ticks uint64) int64 {
	return int64(ticks * 1000 / Rate)
}

// Interval returns cur-prev, or zero if cur did not advance.
func Interval(prev, cur uint64) uint64 {
	if cur <= prev {
		return 0
	}
	return cur - prev
}
