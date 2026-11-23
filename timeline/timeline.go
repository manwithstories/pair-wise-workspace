// Package timeline implements the unified media time base.
//
// Every track — subtitle cues, device timecodes and RTP packet timestamps — is
// converted into the same rational scale (Rate1000: milliseconds with an exact
// fractional remainder) so that cut points across bitrate representations can
// never drift from accumulated float rounding. All arithmetic stays in
// arbitrary-precision integers wrapped in big.Rat/big.Int.
package timeline

import (
	"math/big"

	"mediad/diag"
)

// Rate1000 is the unified tick rate: one whole unit is one millisecond; a
// timestamp of n units equals n/1000 seconds.
const Rate1000 int64 = 1000

// Rat is a rational number of seconds on the unified timeline.
type Rat struct{ r big.Rat }

// NewRat builds a seconds value num/den.
func NewRat(num, den int64) Rat {
	return Rat{r: *new(big.Rat).SetFrac(big.NewInt(num), big.NewInt(den))}
}

// FromMillis creates a rational timestamp from whole milliseconds.
func FromMillis(ms int64) Rat {
	return Rat{r: *new(big.Rat).SetFrac(big.NewInt(ms), big.NewInt(Rate1000))}
}

// FromInt represents an integer number of seconds.
func FromInt(sec int64) Rat { return Rat{r: *big.NewRat(sec, 1)} }

// Millis returns the timestamp rounded to whole milliseconds (half away from
// zero, matching big.Rat.FloatString's rounding).
func (t Rat) Millis() int64 {
	num := new(big.Int).Mul(t.r.Num(), big.NewInt(Rate1000))
	den := new(big.Int).Set(t.r.Denom())
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem)
	twice := new(big.Int).Lsh(rem, 1) // 2*rem vs den => half-up test
	if twice.Cmp(den) >= 0 && t.r.Sign() >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// FloorMillis returns the truncated millisecond value and the exact remainder
// as remNum/remDen (of a millisecond), useful for checkpoints and tests.
func (t Rat) FloorMillis() (whole, remNum, remDen int64) {
	num := new(big.Int).Mul(t.r.Num(), big.NewInt(Rate1000))
	den := new(big.Int).Set(t.r.Denom())
	w, rem := new(big.Int), new(big.Int)
	w.QuoRem(num, den, rem)
	return w.Int64(), rem.Int64(), den.Int64()
}

// Cmp compares two timestamps: -1, 0 or +1.
func (t Rat) Cmp(o Rat) int { return t.r.Cmp(&o.r) }

// Add returns t+o.
func (t Rat) Add(o Rat) Rat { return Rat{r: *new(big.Rat).Add(&t.r, &o.r)} }

// Sub returns t-o.
func (t Rat) Sub(o Rat) Rat { return Rat{r: *new(big.Rat).Sub(&t.r, &o.r)} }

// IsZero reports whether the timestamp equals zero.
func (t Rat) IsZero() bool { return t.r.Sign() == 0 }

// String renders the exact fraction, e.g. "33367/100000".
func (t Rat) String() string { return t.r.RatString() }

// FrameRate is a rational frames-per-second value.
type FrameRate struct {
	// Num/Den express fps, e.g. 30000/1001 for NTSC 29.97.
	Num, Den int64
	// Drop is the SMPTE drop-frame flag, only meaningful for 30000/1001.
	Drop bool
	// Name is the canonical label ("24","25","30","30000/1001-df", ...).
	Name string
}

// NominalFPS returns the decimal frame rate as a rational seconds/frame math
// source: duration of one frame = Den/Num seconds.
func (f FrameRate) frameDuration() Rat {
	return NewRat(f.Den, f.Num)
}

// Known frame rates. Drop-frame uses the SMPTE convention for 29.97.
var (
	FR24   = FrameRate{Num: 24, Den: 1, Name: "24"}
	FR25   = FrameRate{Num: 25, Den: 1, Name: "25"}
	FR30   = FrameRate{Num: 30, Den: 1, Name: "30"}
	FR2997 = FrameRate{Num: 30000, Den: 1001, Name: "30000/1001"}
	FR2997DF = FrameRate{Num: 30000, Den: 1001, Drop: true, Name: "30000/1001-df"}
)

// ParseFrameRate resolves a canonical name to a FrameRate.
func ParseFrameRate(name string) (FrameRate, error) {
	switch name {
	case "24":
		return FR24, nil
	case "25":
		return FR25, nil
	case "30":
		return FR30, nil
	case "30000/1001", "29.97":
		return FR2997, nil
	case "30000/1001-df", "29.97-df":
		return FR2997DF, nil
	default:
		return FrameRate{}, diag.New(diag.ETimecode, "", "unknown frame rate %q", name)
	}
}

// FrameToTime converts a zero-based frame index to an exact timestamp.
// For drop-frame rates, the index is a real-frame count (as carried on the
// wire), so no label arithmetic is needed here; label math lives in Timecode.
func FrameToTime(fr FrameRate, frame int64) (Rat, error) {
	if frame < 0 {
		return Rat{}, diag.New(diag.ETimecode, "", "negative frame index %d", frame)
	}
	d := fr.frameDuration()
	r := new(big.Rat).Mul(big.NewRat(frame, 1), &d.r)
	return Rat{r: *r}, nil
}

// TimeToFrame returns the frame index whose start time is at or before t.
func TimeToFrame(fr FrameRate, t Rat) (int64, error) {
	if t.r.Sign() < 0 {
		return 0, diag.New(diag.ETimecode, "", "negative timestamp %s", t.String())
	}
	d := fr.frameDuration()
	q := new(big.Rat).Quo(&t.r, &d.r)
	w := new(big.Int).Quo(q.Num(), q.Denom())
	return w.Int64(), nil
}
