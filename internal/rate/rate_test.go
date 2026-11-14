package rate

import (
	"errors"
	"math"
	"testing"
)

func syntheticModel(id int) SegmentModel {
	const slope = -0.25
	return SegmentModel{
		ID:      id,
		Bits:    LogBitsModel{Intercept: math.Log2(1_000_000) - slope*30, Slope: slope, Samples: 4},
		Quality: LinearModel{Intercept: 55, Slope: -0.5, Samples: 4},
	}
}

func TestFitLogBits(t *testing.T) {
	qps := []int{18, 26, 35, 44}
	bits := make([]uint64, len(qps))
	for i, qp := range qps {
		bits[i] = uint64(math.Round(math.Exp2(20 - 0.2*float64(qp))))
	}
	model := FitLogBits(qps, bits)
	if model.Samples != len(qps) || model.Conservative {
		t.Fatalf("unexpected fallback model: %+v", model)
	}
	for _, qp := range []int{20, 30, 40} {
		want := math.Exp2(20 - 0.2*float64(qp))
		got := float64(model.PredictBits(qp))
		if math.Abs(got-want)/want > 0.01 {
			t.Fatalf("qp %d: got %.0f want %.0f", qp, got, want)
		}
	}
}

func TestAllocateRespectsBudgetAndBounds(t *testing.T) {
	models := []SegmentModel{syntheticModel(1), syntheticModel(2), syntheticModel(3)}
	allocs, err := Allocate(Request{TargetBits: 3_000_000, DurationSec: 3, MinQP: 18, MaxQP: 44, Segments: models})
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, alloc := range allocs {
		if alloc.QP < 18 || alloc.QP > 44 {
			t.Fatalf("QP outside bounds: %+v", alloc)
		}
		total += alloc.PredictedBits
	}
	if total == 0 || math.Abs(float64(total)-3_000_000)/3_000_000 > 0.02 {
		t.Fatalf("predicted total %.0f is too far from budget", float64(total))
	}
}

func TestAllocateInfeasibleReportsMinimum(t *testing.T) {
	models := []SegmentModel{syntheticModel(1), syntheticModel(2)}
	_, err := Allocate(Request{TargetBits: 10, DurationSec: 2, MinQP: 18, MaxQP: 44, Segments: models})
	if !errors.Is(err, ErrInfeasible) {
		t.Fatalf("got %v, want ErrInfeasible", err)
	}
	var infeasible *InfeasibleError
	if !errors.As(err, &infeasible) || infeasible.MinimumBPS <= 0 {
		t.Fatalf("minimum bitrate missing: %+v", err)
	}
}
