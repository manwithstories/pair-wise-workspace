// Package clock defines the shared rational time base used to align
// tracks that were captured on different machines and clock domains.
//
// All timeline arithmetic is exact (rational) so that repeated runs over
// the same input produce bit-identical ordering and output.
package clock

import (
	"fmt"
	"math/bits"
)

// Rat is an exact rational number N/D, normalized so that D > 0 and
// gcd(|N|, D) == 1. The zero value is 0/1.
type Rat struct {
	N int64
	D int64
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
	if a == 0 {
		return 1
	}
	return a
}

// New returns the normalized rational n/d. It panics on d == 0.
func New(n, d int64) Rat {
	if d == 0 {
		panic("clock: zero denominator")
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd(n, d)
	return Rat{N: n / g, D: d / g}
}

// Add returns r + o. Cross terms are reduced by gcd before
// multiplication to avoid int64 overflow for the magnitudes used here
// (timeline values are kept relative to a session base, so |N| stays
// well below 2^63 after reduction).
func (r Rat) Add(o Rat) Rat {
	g := gcd(r.D, o.D)
	rd, od := r.D/g, o.D/g
	return New(r.N*od+o.N*rd, r.D*od)
}

// Sub returns r - o.
func (r Rat) Sub(o Rat) Rat {
	g := gcd(r.D, o.D)
	rd, od := r.D/g, o.D/g
	return New(r.N*od-o.N*rd, r.D*od)
}

// Mul returns r * o.
func (r Rat) Mul(o Rat) Rat { return New(r.N*o.N, r.D*o.D) }

// Neg returns -r.
func (r Rat) Neg() Rat { return Rat{N: -r.N, D: r.D} }

// Sign returns -1, 0 or +1.
func (r Rat) Sign() int {
	switch {
	case r.N < 0:
		return -1
	case r.N > 0:
		return 1
	default:
		return 0
	}
}

// Cmp compares r and o exactly using 128-bit cross multiplication,
// returning -1, 0 or +1. It never overflows for values produced by this
// pipeline (|N| and D stay well below 2^63 individually).
func (r Rat) Cmp(o Rat) int {
	sa, sb := r.Sign(), o.Sign()
	if sa != sb {
		if sa < sb {
			return -1
		}
		return 1
	}
	if sa == 0 {
		return 0
	}
	// Compare |r.N|*o.D vs |o.N|*r.D as unsigned 128-bit products.
	an := uint64(r.N)
	if r.N < 0 {
		an = uint64(-r.N)
	}
	bn := uint64(o.N)
	if o.N < 0 {
		bn = uint64(-o.N)
	}
	lh, ll := bits.Mul64(an, uint64(o.D))
	rh, rl := bits.Mul64(bn, uint64(r.D))
	var c int
	switch {
	case lh != rh:
		if lh < rh {
			c = -1
		} else {
			c = 1
		}
	case ll != rl:
		if ll < rl {
			c = -1
		} else {
			c = 1
		}
	}
	if sa < 0 {
		c = -c
	}
	return c
}

// Float approximates r as seconds; used only for thresholds and reports,
// never for ordering decisions that must be exact.
func (r Rat) Float() float64 { return float64(r.N) / float64(r.D) }

func (r Rat) String() string {
	if r.D == 0 {
		return fmt.Sprintf("%d/0", r.N)
	}
	return fmt.Sprintf("%d/%d", r.N, r.D)
}

// MediaClock converts RTP timestamp ticks of one track into seconds.
type MediaClock struct {
	// Rate is the RTP clock rate in Hz (e.g. 48000 for audio).
	Rate uint32
}

// Ticks converts an RTP timestamp delta (ticks) into seconds.
func (c MediaClock) Ticks(delta int64) Rat {
	if c.Rate == 0 {
		return New(0, 1)
	}
	return New(delta, int64(c.Rate))
}

// Calibration maps one track's RTP timestamp domain onto the shared
// timeline using an RTCP sender-report anchor:
//
//	shared(ts) = anchorNTP + (ts - anchorRTP) / rate
type Calibration struct {
	Rate      uint32
	AnchorRTP uint32
	AnchorNTP Rat // shared-timeline seconds at AnchorRTP
}

// NewCalibration builds a Calibration from an RTCP sender report.
func NewCalibration(rate, anchorRTP uint32, ntpSeconds Rat) Calibration {
	return Calibration{Rate: rate, AnchorRTP: anchorRTP, AnchorNTP: ntpSeconds}
}

// ToShared maps an RTP timestamp to shared-timeline seconds. The delta is
// computed on the raw 32-bit timestamp space so wrap-around of the RTP
// timestamp itself stays correct.
func (c Calibration) ToShared(ts uint32) Rat {
	delta := int64(int32(ts - c.AnchorRTP))
	return c.AnchorNTP.Add(MediaClock{Rate: c.Rate}.Ticks(delta))
}
