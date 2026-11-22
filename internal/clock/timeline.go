// Package clock provides the single nanosecond time base used by every
// adapter, the remux kernel and the verifier. All conversions are explicit and
// overflow checked so that a hostile or damaged source cannot silently wrap a
// timestamp into the wrong century.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sync"
)

// NSPerSecond is the resolution of the unified timeline.
const NSPerSecond int64 = 1_000_000_000

// Hard limits from the delivery specification.
const (
	MaxAssetDurationNanos int64 = 24 * 60 * 60 * NSPerSecond
	MaxPacketsPerTrack    int64 = 20_000_000
	MaxBufferBytes        int64 = 256 << 20
)

var (
	// ErrTimescale is returned for a zero or otherwise unusable timescale.
	ErrTimescale = errors.New("clock: invalid timescale")
	// ErrOverflow is returned when a value cannot be represented in the
	// unified nanosecond range.
	ErrOverflow = errors.New("clock: value overflows nanosecond range")
)

// Scale is a validated media timescale (units per second) such as 90000 for
// MPEG video or 48000 for PCM audio.
type Scale struct {
	v uint32
}

// NewScale validates and wraps a raw timescale.
func NewScale(v uint32) (Scale, error) {
	if v == 0 {
		return Scale{}, ErrTimescale
	}
	return Scale{v: v}, nil
}

// MustScale is intended for constants known to be valid.
func MustScale(v uint32) Scale {
	s, err := NewScale(v)
	if err != nil {
		panic(err)
	}
	return s
}

// Value returns the raw units-per-second value.
func (s Scale) Value() uint32 { return s.v }

// Valid reports whether the scale can be used.
func (s Scale) Valid() bool { return s.v != 0 }

// Nanos converts a media-time value to nanoseconds, rounding half away from
// zero. It never wraps: values outside the representable range return
// ErrOverflow.
func (s Scale) Nanos(units int64) (int64, error) {
	if s.v == 0 {
		return 0, ErrTimescale
	}
	if units == 0 {
		return 0, nil
	}
	neg := units < 0
	var mag uint64
	if neg {
		if units == math.MinInt64 {
			return 0, ErrOverflow
		}
		mag = uint64(-units)
	} else {
		mag = uint64(units)
	}
	hi, lo := bits.Mul64(mag, uint64(NSPerSecond))
	if hi != 0 || lo > uint64(math.MaxInt64) {
		return 0, ErrOverflow
	}
	q := lo / uint64(s.v)
	r := lo % uint64(s.v)
	if r >= uint64(s.v)-r { // round half up
		q++
	}
	if q > uint64(math.MaxInt64) {
		return 0, ErrOverflow
	}
	out := int64(q)
	if neg {
		out = -out
	}
	return out, nil
}

// Units converts nanoseconds back to media units, rounding half away from zero.
func (s Scale) Units(ns int64) (int64, error) {
	if s.v == 0 {
		return 0, ErrTimescale
	}
	if ns == math.MinInt64 {
		return 0, ErrOverflow
	}
	neg := ns < 0
	mag := uint64(ns)
	if neg {
		mag = uint64(-ns)
	}
	q := mag / uint64(NSPerSecond) * uint64(s.v)
	r := mag % uint64(NSPerSecond)
	hi, lo := bits.Mul64(r, uint64(s.v))
	if hi != 0 || q > math.MaxUint64-lo {
		return 0, ErrOverflow
	}
	q += lo / uint64(NSPerSecond)
	r2 := lo % uint64(NSPerSecond)
	if r2 >= uint64(NSPerSecond)-r2 {
		q++
	}
	if q > uint64(math.MaxInt64) {
		return 0, ErrOverflow
	}
	out := int64(q)
	if neg {
		out = -out
	}
	return out, nil
}

// SignedDelta32 returns the signed forward difference between two 32-bit
// timestamps, naturally unwrapping the RTP 2^32 counter space.
func SignedDelta32(prev, cur uint32) int64 {
	return int64(int32(cur - prev))
}

// Timeline maps a track's local nanosecond value onto the asset-wide timeline.
type Timeline struct {
	origin int64
}

// NewTimeline creates a timeline whose zero point sits at origin (nanoseconds).
func NewTimeline(origin int64) Timeline { return Timeline{origin: origin} }

// At maps a local nanosecond value onto the asset timeline.
func (t Timeline) At(local int64) int64 { return local - t.origin }

// Origin returns the asset origin offset.
func (t Timeline) Origin() int64 { return t.origin }

// ValidateDuration rejects assets longer than the 24 hour delivery cap.
func ValidateDuration(d int64) error {
	if d < 0 || d > MaxAssetDurationNanos {
		return fmt.Errorf("%w: duration %dns", ErrOverflow, d)
	}
	return nil
}

// ValidatePacketCount rejects tracks with more than 20M packets.
func ValidatePacketCount(n int64) error {
	if n < 0 || n > MaxPacketsPerTrack {
		return fmt.Errorf("%w: packet count %d", ErrOverflow, n)
	}
	return nil
}

// Budget is a concurrency-safe byte budget used to enforce the 256 MiB
// buffering cap across adapters.
type Budget struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

// NewBudget creates a budget with the given byte limit.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// Reserve accounts for n bytes or fails with ErrOverflow-like error.
func (b *Budget) Reserve(n int64) error {
	if n < 0 {
		return fmt.Errorf("%w: negative reservation %d", ErrOverflow, n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return fmt.Errorf("%w: buffer cap %d bytes exceeded", ErrOverflow, b.limit)
	}
	b.used += n
	return nil
}

// Release returns n bytes to the budget.
func (b *Budget) Release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
}

// Used reports currently reserved bytes.
func (b *Budget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Limit reports the configured cap.
func (b *Budget) Limit() int64 { return b.limit }
