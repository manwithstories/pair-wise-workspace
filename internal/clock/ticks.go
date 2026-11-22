// Package clock normalises the many timestamp domains found in broadcast
// material (MP4 media timescales, RTP clock rates) onto one microsecond
// timeline. It resolves 32-bit wraparound, negative or non-zero starts and
// reports the first sample that cannot be converted unambiguously.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"medialine/internal/fault"
)

// ErrTimescale is returned when a zero timescale would make conversion
// undefined.
var ErrTimescale = errors.New("timescale must be non-zero")

// TicksToMicros converts ticks in the given timescale to microseconds,
// rounding half away from zero. It returns an error instead of silently
// overflowing when the result does not fit in an int64.
func TicksToMicros(ticks int64, timescale uint64) (int64, error) {
	if timescale == 0 {
		return 0, ErrTimescale
	}
	return mulDiv(ticks, 1_000_000, int64(timescale))
}

// MicrosToTicks is the inverse of TicksToMicros, rounding half away from zero.
func MicrosToTicks(us int64, timescale uint64) (int64, error) {
	if timescale == 0 {
		return 0, ErrTimescale
	}
	return mulDiv(us, int64(timescale), 1_000_000)
}

// mulDiv computes a*b/c with a 128-bit intermediate product and returns an
// error on overflow or division by zero.
func mulDiv(a, b, c int64) (int64, error) {
	if c == 0 {
		return 0, ErrTimescale
	}
	neg := (a < 0) != (b < 0) != (c < 0)
	ua, ub, uc := abs64(a), abs64(b), abs64(c)

	hi, lo := bits.Mul64(ua, ub)
	if hi >= uc {
		return 0, fmt.Errorf("timestamp overflow: %d*%d/%d", a, b, c)
	}
	q, _ := bits.Div64(hi, lo, uc)
	if q > math.MaxInt64 {
		return 0, fmt.Errorf("timestamp overflow: %d*%d/%d", a, b, c)
	}
	if neg {
		return -int64(q), nil
	}
	return int64(q), nil
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

// Unwrapper turns wrapping unsigned timestamps (for example RTP's 32-bit
// counter) into a monotonically increasing signed tick value. Bits=0 disables
// unwrapping and treats values as already absolute, which is what MP4
// baseMediaDecodeTime requires.
type Unwrapper struct {
	bits   uint32
	period uint64
	half   uint64
	init   bool
	last   uint64
	epoch  int64
}

// NewUnwrapper builds an unwrapper for a counter of the given bit width. Only
// 1..32 bit counters are meaningful; 0 disables wraparound handling.
func NewUnwrapper(bitWidth uint32) (*Unwrapper, error) {
	if bitWidth > 32 {
		return nil, fmt.Errorf("unsupported timestamp width %d (max 32)", bitWidth)
	}
	u := &Unwrapper{bits: bitWidth}
	if bitWidth > 0 {
		u.period = uint64(1) << bitWidth
		u.half = u.period / 2
	}
	return u, nil
}

// Unwrap feeds the next raw counter value and returns the absolute tick.
// wrapped reports that the algorithm crossed the counter's wrap point. A
// backwards jump larger than half the counter period is treated as corruption
// rather than a wrap.
func (u *Unwrapper) Unwrap(raw uint64) (abs int64, wrapped bool, err error) {
	if u.bits > 0 {
		raw &= u.period - 1
	}
	if !u.init {
		u.init = true
		u.last = raw
		u.epoch = int64(raw)
		return int64(raw), false, nil
	}
	if u.bits == 0 {
		u.last = raw
		return int64(raw), false, nil
	}

	var delta int64
	if raw >= u.last {
		delta = int64(raw - u.last)
	} else {
		back := u.last - raw
		if back > u.half {
			// The counter ran past its maximum and restarted near zero.
			delta = int64(u.period - back)
			wrapped = true
		} else {
			delta = -int64(back)
		}
	}
	u.last = raw
	u.epoch += delta
	return u.epoch, wrapped, nil
}

// Converter binds a timescale, an unwrap width and a starting time to a
// single track. It is not safe for concurrent use; the pipeline gives each
// track its own converter.
type Converter struct {
	timescale uint64
	unwrapper *Unwrapper
	startUS   int64
	firstTick int64
	haveFirst bool
	prevUS    int64
	prevRaw   uint64
	havePrev  bool
	strict    bool
	samples   int64
}

// Options controls duplicate handling and the timeline origin.
type Options struct {
	// Timescale is the source clock rate in ticks per second.
	Timescale uint64
	// WrapBits is the counter width (32 for RTP, 0 for MP4 media time).
	WrapBits uint32
	// StartUS is the microsecond value assigned to the first sample. It may be
	// negative to model pre-roll or edit-list offsets.
	StartUS int64
	// AllowDuplicates keeps equal consecutive timestamps instead of raising a
	// fault. Duplicates are otherwise reported as the first anomalous sample.
	AllowDuplicates bool
}

// New builds a converter for one track.
func New(opts Options) (*Converter, error) {
	unw, err := NewUnwrapper(opts.WrapBits)
	if err != nil {
		return nil, err
	}
	if opts.Timescale == 0 {
		return nil, ErrTimescale
	}
	return &Converter{timescale: opts.Timescale, unwrapper: unw, startUS: opts.StartUS, strict: !opts.AllowDuplicates}, nil
}

// Convert maps one raw timestamp to microseconds on the unified timeline.
// The first sample defines the origin, so non-zero starts are normalised while
// preserving the configured StartUS.
func (c *Converter) Convert(raw uint64) (int64, error) {
	rawMasked := raw
	if c.unwrapper.bits > 0 {
		rawMasked &= uint64(1)<<c.unwrapper.bits - 1
	}
	if c.havePrev && rawMasked == c.prevRaw && c.strict {
		return 0, fault.New(fault.StageClock, fault.KindDuplicateTime,
			"duplicate timestamp %d at sample %d", rawMasked, c.samples)
	}
	abs, _, err := c.unwrapper.Unwrap(raw)
	if err != nil {
		return 0, fault.From(err, fault.StageClock, fault.KindAmbiguousTime).At(-1, c.samples)
	}
	if !c.haveFirst {
		c.haveFirst = true
		c.firstTick = abs
	}
	delta := abs - c.firstTick
	us, err := TicksToMicros(delta, c.timescale)
	if err != nil {
		return 0, fault.From(err, fault.StageClock, fault.KindAmbiguousTime).At(-1, c.samples)
	}
	us += c.startUS
	if c.havePrev && us < c.prevUS {
		return 0, fault.New(fault.StageClock, fault.KindBackwardsTime,
			"timestamp goes backwards: %dus after %dus", us, c.prevUS).At(-1, c.samples)
	}
	c.prevUS = us
	c.prevRaw = rawMasked
	c.havePrev = true
	c.samples++
	return us, nil
}

// Samples reports how many timestamps have been converted.
func (c *Converter) Samples() int64 { return c.samples }

// Period returns the wrap period in ticks (0 when unwrapping is disabled).
func (u *Unwrapper) Period() uint64 { return u.period }
