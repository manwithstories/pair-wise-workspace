package media

import (
	"errors"
	"math"
	"math/bits"
	"sort"
)

// TickRate is the unified presentation time base used across all cameras.
// 90 kHz is the MPEG clock and divides evenly into 30/60 fps frames.
const TickRate int64 = 90000

// Wrap33 is the modulus of the 33-bit MPEG-2 PTS/DTS counter.
const Wrap33 int64 = 1 << 33

// ErrTimeOverflow is returned when a rescale cannot be represented in int64.
var ErrTimeOverflow = errors.New("timebase: overflow")

// Rational is an exact fraction Num/Den with Den > 0, kept in lowest terms.
// It is the single representation of rates and drift across the kernel.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewRational reduces num/den. A zero denominator is rejected.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, errors.New("timebase: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}, nil
	}
	g := gcd(abs64(num), den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// MustRational is NewRational for compile-time constants.
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Float returns the fraction as a float64.
func (r Rational) Float() float64 { return float64(r.Num) / float64(r.Den) }

// Mul scales r by n, reducing the result.
func (r Rational) Mul(n int64) Rational {
	out, err := NewRational(r.Num*n, r.Den)
	if err != nil {
		return r
	}
	return out
}

// Inverse returns Den/Num.
func (r Rational) Inverse() (Rational, error) { return NewRational(r.Den, r.Num) }

// RescaleTicks applies r to n ticks, rounding half away from zero.
func (r Rational) RescaleTicks(n int64) (int64, error) {
	return mulDivRound(n, r.Num, r.Den)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// mulDivRound computes round(n*num/den) with a 128-bit intermediate so no
// overflow occurs for realistic media timestamps.
func mulDivRound(n, num, den int64) (int64, error) {
	if den == 0 {
		return 0, errors.New("timebase: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	neg := (n < 0) != (num < 0)
	hi, lo := bits.Mul64(uint64(abs64(n)), uint64(abs64(num)))
	d := uint64(den)
	if hi >= d {
		return 0, ErrTimeOverflow
	}
	q, r := bits.Div64(hi, lo, d)
	if r >= d-r { // round half away from zero
		q++
	}
	if q > math.MaxInt64 {
		return 0, ErrTimeOverflow
	}
	v := int64(q)
	if neg {
		v = -v
	}
	return v, nil
}

// Rescale converts v from one timescale to another using exact rational math
// with half-away-from-zero rounding.
func Rescale(v int64, from, to int64) (int64, error) {
	if from == to {
		return v, nil
	}
	if from <= 0 || to <= 0 {
		return 0, errors.New("timebase: invalid timescale")
	}
	return mulDivRound(v, to, from)
}

// TicksToUnified converts a source tick value to the unified 90 kHz axis.
func TicksToUnified(v int64, timescale int64) (int64, error) {
	return Rescale(v, timescale, TickRate)
}

// UnifiedToTicks converts unified ticks back into a source timescale.
func UnifiedToTicks(v int64, timescale int64) (int64, error) {
	return Rescale(v, TickRate, timescale)
}

// UnwrapTimestamp expands a 33-bit raw timestamp into an unbounded tick value
// closest to ref. raw is masked to 33 bits. This is the only place 33-bit wrap
// handling is implemented, so RTP, TS and fMP4 paths agree. ref may be negative.
func UnwrapTimestamp(ref int64, raw int64) int64 {
	const wrap = Wrap33
	r := raw & (wrap - 1)
	refMod := ref % wrap
	if refMod < 0 {
		refMod += wrap
	}
	diff := r - refMod
	if diff > wrap/2 {
		diff -= wrap
	} else if diff < -wrap/2 {
		diff += wrap
	}
	return ref + diff
}

// Unwrap32Timestamp expands a 32-bit RTP timestamp using the same rule.
func Unwrap32Timestamp(ref int64, raw uint32) int64 {
	const wrap = int64(1) << 32
	r := int64(raw)
	refMod := ref % wrap
	if refMod < 0 {
		refMod += wrap
	}
	diff := r - refMod
	if diff > wrap/2 {
		diff -= wrap
	} else if diff < -wrap/2 {
		diff += wrap
	}
	return ref + diff
}

// DriftFit is the robust linear model ref = slope*local + intercept, where
// slope deviates from 1 by the camera clock drift.
type DriftFit struct {
	SlopePPM       float64 `json:"slope_ppm"`
	InterceptTicks int64   `json:"intercept_ticks"`
	AnchorRef      int64   `json:"anchor_ref"`
	AnchorLocal    int64   `json:"anchor_local"`
	BoundaryErrMS  float64 `json:"boundary_err_ms"`
	MaxErrMS       float64 `json:"max_err_ms"`
	Inliers        int     `json:"inliers"`
	Outliers       int     `json:"outliers"`
}

// DriftEstimator accumulates (reference, local) tick pairs for one camera and
// fits a drift model robust to isolated clock jumps. Each estimator is
// per-track, so one camera's outliers cannot contaminate another track.
type DriftEstimator struct {
	ref   []int64
	local []int64
}

// Add records a paired observation in acquisition order.
func (d *DriftEstimator) Add(ref, local int64) {
	d.ref = append(d.ref, ref)
	d.local = append(d.local, local)
}

// Len reports the number of observations.
func (d *DriftEstimator) Len() int { return len(d.ref) }

// Fit computes a Theil-Sen median-slope line, rejects points whose residual
// exceeds six median absolute deviations, then refits inliers by least
// squares. This tolerates clock jumps and VFR without a nominal frame rate.
func (d *DriftEstimator) Fit() (DriftFit, error) {
	n := len(d.ref)
	if n < 2 {
		return DriftFit{}, errors.New("timebase: insufficient drift observations")
	}
	slopes := make([]float64, 0, n*(n-1)/2)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			dl := d.local[j] - d.local[i]
			if dl == 0 {
				continue
			}
			slopes = append(slopes, float64(d.ref[j]-d.ref[i])/float64(dl))
		}
	}
	if len(slopes) == 0 {
		return DriftFit{}, errors.New("timebase: degenerate drift samples")
	}
	sort.Float64s(slopes)
	// Theil-Sen slope: a single bad point perturbs only n-1 of the n(n-1)/2
	// pairwise slopes, so the median is unaffected by isolated jumps.
	slope := medianSorted(slopes)

	// Residuals against the median-slope line anchored at the median point.
	medLocal := medianSorted(append([]int64(nil), d.local...))
	medRef := medianSorted(append([]int64(nil), d.ref...))
	resid := make([]float64, n)
	absResid := make([]float64, n)
	for i := range resid {
		resid[i] = float64(d.ref[i]) - (float64(medRef) + slope*float64(d.local[i]-medLocal))
		absResid[i] = math.Abs(resid[i])
	}
	mad := medianSorted(append([]float64(nil), absResid...))
	threshold := 6*mad + 1000
	if threshold < 3000 { // never reject below one frame of jitter
		threshold = 3000
	}
	inliers := 0
	for i := range absResid {
		if absResid[i] <= threshold {
			inliers++
		}
	}
	if inliers < 2 {
		inliers = n
	}
	intercept := float64(medRef) - slope*float64(medLocal)
	if inliers >= 2 {
		var sumX, sumY, sumXX, sumXY float64
		cnt := 0
		for i := range absResid {
			if absResid[i] > threshold {
				continue
			}
			x, y := float64(d.local[i]), float64(d.ref[i])
			sumX += x
			sumY += y
			sumXX += x * x
			sumXY += x * y
			cnt++
		}
		fn := float64(cnt)
		if den := fn*sumXX - sumX*sumX; den != 0 {
			slope = (fn*sumXY - sumX*sumY) / den
			intercept = (sumY - slope*sumX) / fn
		}
	}
	fit := DriftFit{
		SlopePPM:       (slope - 1) * 1e6,
		InterceptTicks: int64(math.Round(intercept)),
		Inliers:        inliers,
		Outliers:       n - inliers,
	}
	maxErr := 0.0
	for i := range absResid {
		pred := slope*float64(d.local[i]) + intercept
		e := math.Abs(float64(d.ref[i]) - pred)
		if e > maxErr {
			maxErr = e
		}
		if absResid[i] <= threshold && fit.AnchorLocal == 0 && fit.AnchorRef == 0 {
			fit.AnchorLocal = d.local[i]
			fit.AnchorRef = d.ref[i]
		}
	}
	fit.MaxErrMS = maxErr / (float64(TickRate) / 1000)
	fit.BoundaryErrMS = fit.MaxErrMS
	return fit, nil
}

func medianSorted[T int64 | float64](v []T) T {
	if len(v) == 0 {
		return 0
	}
	if len(v) == 1 {
		return v[0]
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	mid := len(v) / 2
	if len(v)%2 == 1 {
		return v[mid]
	}
	return (v[mid-1] + v[mid]) / 2
}
