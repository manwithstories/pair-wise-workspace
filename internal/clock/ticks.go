// Package clock normalises every stream onto one integer timebase using
// rational-number integer arithmetic. All multiplications are checked for
// int64 overflow: a rejected conversion becomes a model.INTEGER_OVERFLOW
// record instead of a silently wrapped duration.
package clock

import (
	"errors"
	"fmt"
)

// MasterHz is the unified pipeline tick rate (MPEG-2 TS / PTS clock base).
// Every stream timebase is converted to MasterHz ticks before timeline
// composition, rendering and verification.
const MasterHz int64 = 90000

// Sentinel errors returned by the checked arithmetic helpers.
var (
	ErrOverflow = errors.New("integer tick conversion overflow")
	ErrBadRate  = errors.New("invalid rational timebase")
)

// Rate is an integer timebase expressed as Num ticks per Den seconds,
// i.e. rate = Num/Den Hz. Both stay integers so no float ever enters
// the time path.
type Rate struct {
	Num int64
	Den int64
}

// NewRate validates and builds a Rate.
func NewRate(num, den int64) (Rate, error) {
	if num <= 0 || den <= 0 {
		return Rate{}, fmt.Errorf("%w: num=%d den=%d", ErrBadRate, num, den)
	}
	return Rate{Num: num, Den: den}, nil
}

// mul64 returns a*b with an overflow check (wrapping multiplication is
// never allowed on the time path).
func mul64(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	r := a * b
	if r/b != a {
		return 0, fmt.Errorf("%w: %d * %d", ErrOverflow, a, b)
	}
	return r, nil
}

// add64 returns a+b with an overflow check.
func add64(a, b int64) (int64, error) {
	r := a + b
	if (b > 0 && r < a) || (b < 0 && r > a) {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return r, nil
}

// ToMaster converts ticks expressed in rate r into MasterHz ticks:
//
//	master = ticks * MasterHz * r.Den / r.Num
//
// Division happens last to keep the rounding error below one master tick.
func (r Rate) ToMaster(ticks int64) (int64, error) {
	if r.Num <= 0 || r.Den <= 0 {
		return 0, ErrBadRate
	}
	scaled, err := mul64(ticks, MasterHz)
	if err != nil {
		return 0, err
	}
	scaled, err = mul64(scaled, r.Den)
	if err != nil {
		return 0, err
	}
	return scaled / r.Num, nil
}

// FromMaster converts MasterHz ticks into rate r (inverse of ToMaster).
func (r Rate) FromMaster(masterTicks int64) (int64, error) {
	if r.Num <= 0 || r.Den <= 0 {
		return 0, ErrBadRate
	}
	scaled, err := mul64(masterTicks, r.Num)
	if err != nil {
		return 0, err
	}
	return scaled / (MasterHz * r.Den), nil
}

// Convert moves ticks from one rate directly onto another, still in pure
// integers: out = ticks * from.Den * to.Num / (from.Num * to.Den).
func Convert(ticks int64, from, to Rate) (int64, error) {
	if from.Num <= 0 || to.Num <= 0 || from.Den <= 0 || to.Den <= 0 {
		return 0, ErrBadRate
	}
	num, err := mul64(from.Den, to.Num)
	if err != nil {
		return 0, err
	}
	den, err := mul64(from.Num, to.Den)
	if err != nil {
		return 0, err
	}
	scaled, err := mul64(ticks, num)
	if err != nil {
		return 0, err
	}
	return scaled / den, nil
}

// MasterToMs renders master ticks as whole milliseconds for reports.
func MasterToMs(masterTicks int64) int64 {
	return masterTicks * 1000 / MasterHz
}

// MsToMaster converts a millisecond duration into master ticks.
func MsToMaster(ms int64) (int64, error) {
	return mul64(ms, MasterHz/1000)
}
