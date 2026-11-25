package media

import (
	"fmt"
	"math"
)

// Rational is an exact rational time value. Num and Den are kept reduced by
// Normalize; Den is always positive after construction.
type Rational struct {
	Num int64
	Den int64
}

// CommonTimebase is the single presentation tick rate shared by every track.
// 90 kHz is the conventional MPEG time base and exactly covers the synthetic
// video durations used by the fixture generator.
var CommonTimebase = Rational{Num: 1, Den: 90_000}

// Rat constructs and normalizes a rational.
func Rat(num, den int64) Rational {
	if den == 0 {
		panic("media: zero rational denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}
	}
	g := gcd(abs64(num), den)
	return Rational{Num: num / g, Den: den / g}
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}

// Normalize returns the reduced rational.
func (r Rational) Normalize() Rational { return Rat(r.Num, r.Den) }

// Float64 is used only for human-readable reporting.
func (r Rational) Float64() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Cmp compares two rationals without floating-point loss.
func (r Rational) Cmp(o Rational) int {
	r = r.Normalize()
	o = o.Normalize()
	l := r.Num * o.Den
	rr := o.Num * r.Den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	default:
		return 0
	}
}

// Add returns r+o.
func (r Rational) Add(o Rational) Rational {
	return Rat(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

// Sub returns r-o.
func (r Rational) Sub(o Rational) Rational {
	return Rat(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Scaled converts a value measured in timescale ticks into rational seconds.
func Scaled(value int64, timescale uint32) Rational {
	if timescale == 0 {
		panic("media: zero timescale")
	}
	return Rat(value, int64(timescale))
}

// ToTicks converts rational seconds to ticks of tb. Rounding is nearest-even
// away from the half boundary and is recorded by callers when adjustment matters.
func ToTicks(t Rational, tb Rational) int64 {
	t = t.Normalize()
	tb = tb.Normalize()
	if tb.Num == 0 {
		return 0
	}
	// t / tb = (t.Num*t.Den? no): (t.Num/t.Den)/(tb.Num/tb.Den)
	num := t.Num * tb.Den
	den := t.Den * tb.Num
	q, rem := num/den, num%den
	if rem != 0 && abs64(rem)*2 >= den {
		if num > 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

// ConvertTicks converts an integer tick value from one rational base to another.
func ConvertTicks(value int64, from, to Rational) int64 {
	return ToTicks(Rat(value*from.Num, from.Den), to)
}

// UnwrapUint resolves a wrapping unsigned counter. last is the highest
// already-unwrapped value; first callers should pass ok=false.
func UnwrapUint(bits uint, last uint64, value uint64, haveLast bool) uint64 {
	if bits == 0 || bits >= 64 {
		return value
	}
	mod := uint64(1) << bits
	mask := mod - 1
	value &= mask
	if !haveLast {
		return value
	}
	half := int64(mod / 2)
	delta := int64(value) - int64(last&mask)
	if delta > half {
		delta -= int64(mod)
	}
	if delta < -half {
		delta += int64(mod)
	}
	return uint64(int64(last) + delta)
}

// ResetKind explains how Timeline resolved a backward raw timestamp.
type ResetKind string

const (
	ResetNone          ResetKind = ""
	ResetDiscontinuity ResetKind = "discontinuity"
	ResetTFDTFallback  ResetKind = "tfdt_fallback_corrected"
)

// TimelineEvent records a non-monotonic timestamp resolution.
type TimelineEvent struct {
	Track    string
	Kind     ResetKind
	RawTicks int64
	Common   int64
	Message  string
}

type trackClock struct {
	lastRaw     int64
	lastCommon  int64
	epochOffset int64
	have        bool
}

// Timeline enforces one common tick base while preserving evidence of resets.
type Timeline struct {
	Base   Rational
	tracks map[string]*trackClock
	Events []TimelineEvent
}

// NewTimeline creates a common-clock resolver.
func NewTimeline() *Timeline {
	return &Timeline{Base: CommonTimebase, tracks: make(map[string]*trackClock)}
}

// Resolve maps a track-local scaled timestamp to common ticks. An explicit
// discontinuity starts a new device epoch but is offset onto the continuous
// repair timeline. An unexpected backward timestamp is corrected forward and
// reported rather than silently dropping the media sample.
func (tl *Timeline) Resolve(track string, raw int64, timescale uint32, explicitDiscontinuity bool) (int64, *TimelineEvent) {
	st := tl.tracks[track]
	if st == nil {
		st = &trackClock{}
		tl.tracks[track] = st
	}
	rawCommon := ConvertTicks(raw, Rational{Num: 1, Den: int64(timescale)}, tl.Base)
	if !st.have {
		st.have = true
		st.lastRaw = raw
		st.lastCommon = rawCommon
		return rawCommon, nil
	}

	if explicitDiscontinuity && raw < st.lastRaw {
		// Place the new epoch immediately after the previously known instant.
		st.epochOffset = st.lastCommon + 1 - rawCommon
		common := rawCommon + st.epochOffset
		st.lastRaw = raw
		st.lastCommon = common
		ev := TimelineEvent{Track: track, Kind: ResetDiscontinuity, RawTicks: raw, Common: common,
			Message: "explicit discontinuity mapped to continuous common clock"}
		tl.Events = append(tl.Events, ev)
		return common, &ev
	}

	if raw < st.lastRaw {
		// A regressed tfdt is evidence of damage. Preserve monotonic output by
		// advancing to the prior common tick plus the raw delta where possible.
		common := st.lastCommon + max64(1, rawCommon)
		ev := TimelineEvent{Track: track, Kind: ResetTFDTFallback, RawTicks: raw, Common: common,
			Message: fmt.Sprintf("tfdt regressed from %d to %d; corrected without dropping samples", st.lastRaw, raw)}
		tl.Events = append(tl.Events, ev)
		st.lastRaw = raw
		st.lastCommon = common
		return common, &ev
	}

	common := st.epochOffset + rawCommon
	st.lastRaw = raw
	st.lastCommon = common
	return common, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
