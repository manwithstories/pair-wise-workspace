// Package timeline owns the common 90 kHz time base that every camera
// angle and every timed-event stream is converted into.
package timeline

import (
	"math/big"

	"fastpost/internal/model"
)

// CommonRate is the public tick rate of the unified timeline, matching
// the MPEG-2 TS / HLS convention of a 90 kHz clock.
const CommonRate int64 = 90_000

// Rational is an exact time value or frequency expressed as a fraction.
// Den must always be positive.
type Rational struct {
	Num int64
	Den int64
}

// Timebase maps one source track's local clock onto the common timeline.
// Rate is the source clock frequency in Hz (e.g. 48000 for audio samples,
// 30000/1001 for fractional-rate video). Origin is the common-tick value
// at which this track's local time zero starts; it absorbs per-camera
// clock skew so skew_ticks collapses to zero once aligned.
type Timebase struct {
	Track  string
	Rate   Rational
	Origin int64
}

// New validates a timebase and returns it. A non-positive rate is
// reported as an unmappable condition on the named track.
func New(track string, rate Rational, origin int64) (Timebase, error) {
	if rate.Den <= 0 || rate.Num <= 0 {
		return Timebase{}, model.Fault{
			Kind: model.FaultUnmappableEvent, Track: track, Offset: -1,
			Reason: "non-positive timebase rate",
		}
	}
	return Timebase{Track: track, Rate: rate, Origin: origin}, nil
}

// roundedDiv divides num by den with round-half-away-from-zero semantics.
// Callers supply the exact numerator and denominator as integers so that
// no intermediate product can silently overflow.
func roundedDiv(num, den *big.Int) (int64, bool) {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() != 0 {
		cmp := new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2))
		if cmp.Cmp(new(big.Int).Abs(den)) >= 0 {
			if q.Sign() >= 0 {
				q.Add(q, big.NewInt(1))
			} else {
				q.Sub(q, big.NewInt(1))
			}
		}
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

// ToCommon converts a duration measured in the source clock's units into
// common 90 kHz ticks. Negative durations and values that overflow the
// int64 tick range are rejected with structured faults.
func (tb Timebase) ToCommon(localUnits int64) (int64, error) {
	if localUnits < 0 {
		return 0, model.Fault{
			Kind: model.FaultNegativeDuration, Track: tb.Track, Offset: -1,
			Reason: "negative duration in source units",
		}
	}
	// ticks = localUnits * CommonRate * Rate.Den / Rate.Num
	num := new(big.Int).Mul(big.NewInt(localUnits), big.NewInt(CommonRate))
	num.Mul(num, big.NewInt(tb.Rate.Den))
	ticks, ok := roundedDiv(num, big.NewInt(tb.Rate.Num))
	if !ok {
		return 0, model.Fault{
			Kind: model.FaultOverflow, Track: tb.Track, Offset: -1,
			Reason: "conversion to 90 kHz ticks overflows int64",
		}
	}
	return ticks, nil
}

// ToCommonAt converts a source timestamp (absolute position in source
// units) to an absolute common-tick timestamp, including the per-track
// origin offset.
func (tb Timebase) ToCommonAt(localPos int64) (int64, error) {
	if localPos < 0 {
		return 0, model.Fault{
			Kind: model.FaultUnmappableEvent, Track: tb.Track, Offset: -1,
			Reason: "negative source timestamp",
		}
	}
	ticks, err := tb.ToCommon(localPos)
	if err != nil {
		return 0, err
	}
	sum := ticks + tb.Origin
	if (ticks > 0 && sum < tb.Origin) || (tb.Origin > 0 && sum < ticks) {
		return 0, model.Fault{
			Kind: model.FaultOverflow, Track: tb.Track, Offset: -1,
			Reason: "origin-relative timestamp overflows int64",
		}
	}
	return sum, nil
}

// FromCommon converts common ticks back into source units, used when
// shared segment boundaries have to be expressed per variant.
func (tb Timebase) FromCommon(ticks int64) (int64, error) {
	if ticks < 0 {
		return 0, model.Fault{
			Kind: model.FaultNegativeDuration, Track: tb.Track, Offset: -1,
			Reason: "negative common-tick duration",
		}
	}
	// units = ticks * Rate.Num / (CommonRate * Rate.Den)
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(tb.Rate.Num))
	den := new(big.Int).Mul(big.NewInt(CommonRate), big.NewInt(tb.Rate.Den))
	units, ok := roundedDiv(num, den)
	if !ok {
		return 0, model.Fault{
			Kind: model.FaultOverflow, Track: tb.Track, Offset: -1,
			Reason: "conversion back to source units overflows int64",
		}
	}
	return units, nil
}

// Compare returns -1/0/1 for two rationals without float conversion.
func Compare(a, b Rational) int {
	l := new(big.Int).Mul(big.NewInt(a.Num), big.NewInt(b.Den))
	r := new(big.Int).Mul(big.NewInt(b.Num), big.NewInt(a.Den))
	return l.Cmp(r)
}
