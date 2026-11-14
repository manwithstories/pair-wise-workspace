package rate

import (
	"errors"
	"fmt"
	"math"
)

var ErrInfeasible = errors.New("bit budget is infeasible")

type InfeasibleError struct {
	Reason      string
	TargetBits  uint64
	MinimumBits uint64
	MaximumBits uint64
	MinimumBPS  float64
	MaximumBPS  float64
}

func (e *InfeasibleError) Error() string {
	return fmt.Sprintf("%v: %s (minimum achievable %.0f bps)", ErrInfeasible, e.Reason, e.MinimumBPS)
}

func (e *InfeasibleError) Unwrap() error { return ErrInfeasible }

type SegmentModel struct {
	ID      int
	Bits    LogBitsModel
	Quality LinearModel
}

type Request struct {
	TargetBits  uint64
	DurationSec float64
	MinQP       int
	MaxQP       int
	Segments    []SegmentModel
}

type Allocation struct {
	ID            int     `json:"id"`
	QP            int     `json:"qp"`
	PredictedBits uint64  `json:"predicted_bits"`
	PredictedPSNR float64 `json:"predicted_psnr_db"`
}

func Allocate(req Request) ([]Allocation, error) {
	if len(req.Segments) == 0 || req.MinQP < 0 || req.MaxQP > 63 || req.MinQP > req.MaxQP || req.TargetBits == 0 || req.DurationSec <= 0 {
		return nil, errors.New("invalid allocation request")
	}
	minimum := allocateAtQP(req.Segments, req.MaxQP)
	maximum := allocateAtQP(req.Segments, req.MinQP)
	minBits, maxBits := totalBits(minimum), totalBits(maximum)
	bpsScale := 1 / req.DurationSec
	if req.TargetBits < minBits {
		return nil, &InfeasibleError{
			Reason:      "target is below the minimum achievable bitrate",
			TargetBits:  req.TargetBits,
			MinimumBits: minBits,
			MaximumBits: maxBits,
			MinimumBPS:  float64(minBits) * bpsScale,
			MaximumBPS:  float64(maxBits) * bpsScale,
		}
	}
	if req.TargetBits > maxBits {
		return nil, &InfeasibleError{
			Reason:      "target exceeds the maximum bitrate at the minimum QP",
			TargetBits:  req.TargetBits,
			MinimumBits: minBits,
			MaximumBits: maxBits,
			MinimumBPS:  float64(minBits) * bpsScale,
			MaximumBPS:  float64(maxBits) * bpsScale,
		}
	}
	low := qualityAtQP(req.Segments, req.MaxQP)
	high := qualityAtQP(req.Segments, req.MinQP)
	best := minimum
	bestErr := absDiff(totalBits(best), req.TargetBits)
	for iter := 0; iter < 64; iter++ {
		mid := (low + high) / 2
		candidate := allocateQuality(req.Segments, req.MinQP, req.MaxQP, mid)
		bits := totalBits(candidate)
		err := absDiff(bits, req.TargetBits)
		if err < bestErr || (err == bestErr && bits <= req.TargetBits) {
			best, bestErr = candidate, err
		}
		if bits < req.TargetBits {
			low = mid
		} else {
			high = mid
		}
	}
	best = improveIntegerQP(best, req)
	return best, nil
}

func allocateQuality(segments []SegmentModel, minQP, maxQP int, quality float64) []Allocation {
	out := make([]Allocation, len(segments))
	for i, s := range segments {
		qp := s.Quality.QPForPSNR(quality, minQP, maxQP)
		out[i] = Allocation{ID: s.ID, QP: qp, PredictedBits: s.Bits.PredictBits(qp), PredictedPSNR: s.Quality.PredictPSNR(qp)}
	}
	return out
}

func allocateAtQP(segments []SegmentModel, qp int) []Allocation {
	out := make([]Allocation, len(segments))
	for i, s := range segments {
		out[i] = Allocation{ID: s.ID, QP: qp, PredictedBits: s.Bits.PredictBits(qp), PredictedPSNR: s.Quality.PredictPSNR(qp)}
	}
	return out
}

func qualityAtQP(segments []SegmentModel, qp int) float64 {
	var sum float64
	for _, s := range segments {
		sum += s.Quality.PredictPSNR(qp)
	}
	return sum / float64(len(segments))
}

func totalBits(allocs []Allocation) uint64 {
	var total uint64
	for _, a := range allocs {
		total += a.PredictedBits
	}
	return total
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// improveIntegerQP makes a small coordinate-descent pass to reduce the
// residual caused by rounding continuous quality targets to integer QPs.
func improveIntegerQP(best []Allocation, req Request) []Allocation {
	for pass := 0; pass < 2; pass++ {
		changed := false
		baseErr := absDiff(totalBits(best), req.TargetBits)
		for i := range best {
			for _, delta := range []int{-1, 1} {
				qp := best[i].QP + delta
				if qp < req.MinQP || qp > req.MaxQP {
					continue
				}
				s := req.Segments[i]
				oldBits := best[i].PredictedBits
				newBits := s.Bits.PredictBits(qp)
				nextTotal := totalBits(best) - oldBits + newBits
				nextErr := absDiff(nextTotal, req.TargetBits)
				if nextErr < baseErr {
					best[i].QP = qp
					best[i].PredictedBits = newBits
					best[i].PredictedPSNR = s.Quality.PredictPSNR(qp)
					baseErr = nextErr
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	return best
}

func ClampAllocationQP(qp, minQP, maxQP int) int {
	return int(math.Max(float64(minQP), math.Min(float64(maxQP), float64(qp))))
}
