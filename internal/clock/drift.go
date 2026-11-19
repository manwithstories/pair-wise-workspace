package clock

import (
	"math/big"
	"sort"

	"archivearchivist/internal/model"
)

// estimateDrift fits arrival time against normalized ticks through the origin.
// With y in microseconds and x in ticks, e = y*rate - x*1e6 equals
// drift_ppm*x + noise for a clock whose media rate is low by drift_ppm, so ppm
// is the least-squares slope of e over x.
func estimateDrift(points []Point, rate uint32) model.Rational {
	type xy struct{ x, y int64 }
	pts := make([]xy, 0, len(points))
	var t0, a0 int64
	// Arrival time is only comparable to media time when packets are ordered
	// by arrival. Late and reordered packets produce outliers that would bias
	// the slope, so the fit uses only the monotonic arrival subsequence.
	for _, i := range monotonicByArrival(points) {
		p := points[i]
		if len(pts) == 0 {
			t0, a0 = p.Norm, p.ArrivalUS
		}
		pts = append(pts, xy{x: p.Norm - t0, y: p.ArrivalUS - a0})
	}
	if len(pts) < 3 {
		return model.NewRat(0, 1)
	}
	var sxx, sxe big.Int
	for _, p := range pts {
		if p.x == 0 {
			continue
		}
		x := big.NewInt(p.x)
		e := new(big.Int).Mul(big.NewInt(p.y), big.NewInt(int64(rate)))
		e.Sub(e, new(big.Int).Mul(x, big.NewInt(1_000_000)))
		sxx.Add(&sxx, new(big.Int).Mul(x, x))
		sxe.Add(&sxe, new(big.Int).Mul(x, e))
	}
	if sxx.Sign() == 0 {
		return model.NewRat(0, 1)
	}
	// ppm = sxe/sxx, rounded to a 1/1000 ppm resolution. Fractional ppm has
	// no practical meaning beyond that.
	exact := new(big.Rat).SetFrac(&sxe, &sxx)
	milli := new(big.Rat).Mul(exact, big.NewRat(1000, 1))
	num := roundRat(milli)
	if !num.IsInt64() {
		return model.NewRat(0, 1)
	}
	return model.NewRat(num.Int64(), 1000)
}

// roundRat rounds to the nearest integer, halves away from zero.
func roundRat(r *big.Rat) *big.Int {
	q := new(big.Int).Quo(r.Num(), r.Denom())
	rem := new(big.Int).Rem(r.Num(), r.Denom())
	twice := new(big.Int).Abs(rem)
	twice.Lsh(twice, 1)
	if twice.Cmp(new(big.Int).Abs(r.Denom())) >= 0 {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// measureSkew returns the largest absolute residual of arrival time against
// the drift-corrected media line. Only packets that arrive in media order take
// part: a reordered packet's arrival lag is transport jitter, not a clock
// synchronization error, and counting it would conflate the two.
func measureSkew(points []Point, rate uint32, ppm model.Rational) int64 {
	idx := monotonicByArrival(points)
	if len(idx) == 0 {
		return 0
	}
	first := &points[idx[0]]
	var maxSkew int64
	for _, i := range idx {
		p := &points[i]
		mediaUS := TicksToUS(p.Norm-first.Norm, rate, ppm)
		skew := p.ArrivalUS - first.ArrivalUS - mediaUS
		p.SkewUS = skew
		if abs64(skew) > maxSkew {
			maxSkew = abs64(skew)
		}
	}
	return maxSkew
}

// monotonicByArrival returns the indices of points ordered by arrival whose
// media time also increases, starting from the earliest arrival.
func monotonicByArrival(points []Point) []int {
	idx := make([]int, 0, len(points))
	for i := range points {
		if points[i].HasArrival {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return points[idx[a]].ArrivalUS < points[idx[b]].ArrivalUS
	})
	keep := idx[:0]
	var maxNorm int64 = -1
	for _, i := range idx {
		if points[i].Norm <= maxNorm {
			continue
		}
		maxNorm = points[i].Norm
		keep = append(keep, i)
	}
	return keep
}
