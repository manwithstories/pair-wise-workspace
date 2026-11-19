// Package timeline unifies frame rates, clock domains, wrap handling and
// timestamp conversion for every mediasync stage.
package timeline

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"

	"mediasync/internal/media"
)

// Rate is an exact rational frame rate (frames per second).
type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Common broadcast frame rates expressed exactly.
var (
	Rate24     = Rate{24, 1}
	Rate25     = Rate{25, 1}
	Rate30     = Rate{30, 1}
	Rate50     = Rate{50, 1}
	Rate60     = Rate{60, 1}
	Rate23_976 = Rate{24000, 1001}
	Rate29_97  = Rate{30000, 1001}
	Rate59_94  = Rate{60000, 1001}
)

// ParseRate parses "25", "30000/1001", "23.976" and near-NTSC decimals.
func ParseRate(s string) (Rate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rate{}, errRate("empty frame rate")
	}
	if strings.Contains(s, "/") {
		parts := strings.Split(s, "/")
		if len(parts) != 2 {
			return Rate{}, errRate("fraction must be num/den: %q", s)
		}
		num, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return Rate{}, errRate("bad numerator in %q", s)
		}
		den, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			return Rate{}, errRate("bad denominator in %q", s)
		}
		r := Rate{num, den}
		return r.Reduce(), r.Validate()
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return Rate{}, errRate("unrecognised frame rate %q", s)
	}
	for _, known := range []Rate{Rate23_976, Rate29_97, Rate59_94} {
		kv := known.Float()
		if math.Abs(v-kv) < 5e-4 {
			return known, nil
		}
	}
	// Decimal input is interpreted as an exact decimal fraction.
	intPart, fracPart, _ := strings.Cut(strings.TrimPrefix(s, "+"), ".")
	num, _ := strconv.ParseInt(intPart, 10, 64)
	den := int64(1)
	if fracPart != "" {
		for range fracPart {
			num *= 10
			den *= 10
		}
		f, err := strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return Rate{}, errRate("unrecognised frame rate %q", s)
		}
		if intPart != "" && strings.HasPrefix(intPart, "-") {
			num -= f
		} else {
			num += f
		}
	}
	r := Rate{num, den}
	return r.Reduce(), r.Validate()
}

// Reduce normalises a rate to lowest terms with a positive denominator.
func (r Rate) Reduce() Rate {
	if r.Den < 0 {
		r.Num, r.Den = -r.Num, -r.Den
	}
	g := gcd(r.Num, r.Den)
	if g == 0 {
		return r
	}
	return Rate{r.Num / g, r.Den / g}
}

// Validate reports whether the rate is usable.
func (r Rate) Validate() error {
	if r.Num <= 0 || r.Den <= 0 {
		return errRate("frame rate must be positive, got %d/%d", r.Num, r.Den)
	}
	if r.Num > 1_000_000 || r.Den > 1_000_000 {
		return errRate("frame rate out of supported range: %d/%d", r.Num, r.Den)
	}
	return nil
}

// Float returns the numeric frames-per-second value.
func (r Rate) Float() float64 { return float64(r.Num) / float64(r.Den) }

// Nominal returns the rounded integer frame rate used by timecode labels.
func (r Rate) Nominal() int64 { return (r.Num + r.Den/2) / r.Den }

// String renders either an integer rate or a reduced fraction.
func (r Rate) String() string {
	if r.Den == 1 {
		return strconv.FormatInt(r.Num, 10)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// Clock describes a raw timestamp domain: its tick rate and wrap modulus.
type Clock struct {
	Domain  string `json:"domain"`
	Ticks   uint32 `json:"ticks_per_second"`
	Wrap    uint64 `json:"wrap_ticks"`
	EpochNS int64  `json:"epoch_unix_nano"`
}

// Common clock domains.
func RTPClock(ssrc uint32) Clock { return Clock{Domain: fmt.Sprintf("rtp/%08x", ssrc), Ticks: 90000} }
func MP4Clock() Clock            { return Clock{Domain: "mp4/media", Ticks: 0} }

// Timebase binds a frame rate to a clock domain.
type Timebase struct {
	Rate      Rate  `json:"rate"`
	Clock     Clock `json:"clock"`
	DropFrame bool  `json:"drop_frame"`
}

// NewTimebase builds a timebase, defaulting the clock to a 90 kHz MPEG clock.
func NewTimebase(rate Rate, clock Clock) (Timebase, error) {
	if err := rate.Validate(); err != nil {
		return Timebase{}, err
	}
	if clock.Ticks == 0 {
		clock.Ticks = 90000
	}
	if clock.Domain == "" {
		clock.Domain = "local"
	}
	return Timebase{Rate: rate, Clock: clock}, nil
}

// Compatible reports whether two timebases may be compared for dedupe.
// Different frame rates, tick domains or wrap rules are not comparable.
func (a Timebase) Compatible(b Timebase) bool {
	return a.Rate.Reduce() == b.Rate.Reduce() &&
		a.Clock.Ticks == b.Clock.Ticks &&
		a.Clock.Wrap == b.Clock.Wrap &&
		a.Clock.Domain == b.Clock.Domain &&
		a.DropFrame == b.DropFrame
}

// FramesToNanos converts a frame count to nanoseconds, half-up. The product
// is formed in 128 bits so extreme frame counts saturate instead of wrapping.
func (r Rate) FramesToNanos(frames int64) int64 {
	return mulDiv(frames, 1_000_000_000*r.Den, r.Num)
}

// NanosToFrames converts nanoseconds to the nearest whole frame.
func (r Rate) NanosToFrames(ns int64) int64 {
	return mulDiv(ns, r.Num, r.Den*1_000_000_000)
}

// SecondsToFrames converts seconds to the nearest whole frame.
func (r Rate) SecondsToFrames(sec float64) int64 {
	return int64(math.Round(sec * r.Float()))
}

// TicksToNanos converts raw clock ticks to nanoseconds.
func (tb Timebase) TicksToNanos(ticks int64) int64 {
	return mulDiv(ticks, 1_000_000_000, int64(tb.Clock.Ticks))
}

// NanosToTicks converts nanoseconds to raw clock ticks, half-up.
func (tb Timebase) NanosToTicks(ns int64) int64 {
	return mulDiv(ns, int64(tb.Clock.Ticks), 1_000_000_000)
}

// TicksToFrames converts raw clock ticks to frames in this timebase.
func (tb Timebase) TicksToFrames(ticks int64) int64 {
	return tb.Rate.NanosToFrames(tb.TicksToNanos(ticks))
}

// FramesToTicks converts frames to raw clock ticks in this timebase.
func (tb Timebase) FramesToTicks(frames int64) int64 {
	return tb.NanosToTicks(tb.Rate.FramesToNanos(frames))
}

// RoundtripFrames verifies frame -> nanosecond -> frame stability.
func (tb Timebase) RoundtripFrames(frames int64) int64 {
	return tb.Rate.NanosToFrames(tb.Rate.FramesToNanos(frames))
}

func errRate(format string, args ...any) error {
	return media.New(media.StageTimebase, media.InputTC, media.CodeInvalidArgument, format, args...)
}

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// mulDiv computes round-half-up(a*b/c) with a 128-bit intermediate product so
// that large frame counts stay exact on 64-bit platforms.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	neg := false
	if a < 0 {
		a, neg = -a, !neg
	}
	if b < 0 {
		b, neg = -b, !neg
	}
	if c < 0 {
		c, neg = -c, !neg
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	// Round to nearest by adding c/2 before the division.
	if half := uint64(c) / 2; half > 0 {
		var carry uint64
		lo, carry = bits.Add64(lo, half, 0)
		hi += carry
	}
	if hi >= uint64(c) {
		// Quotient would overflow int64; saturate deterministically.
		if neg {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, uint64(c))
	if neg {
		return -int64(q)
	}
	return int64(q)
}
