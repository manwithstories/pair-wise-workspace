package job

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"rcx/internal/codec"
	"rcx/internal/reel"
	"rcx/internal/task"
)

type measuredSegment struct {
	bits uint64
	psnr float64
}

// calibratePlan measures the exact rate at the planned QPs, then shifts whole
// integer QPs using local piecewise rate estimates. Since encoding is
// deterministic, the final re-encode matches the last measurement exactly.
func calibratePlan(ctx context.Context, plan Plan, bundle *reel.Bundle, cfg Config) (Plan, error) {
	const tolerance = 0.006
	measurements := make(map[int]measuredSegment, len(plan.Segments))
	for pass := 0; pass < 4; pass++ {
		measured, err := measurePlan(ctx, plan, bundle, cfg)
		if err != nil {
			return Plan{}, err
		}
		measurements = measured
		var total uint64
		for _, sp := range plan.Segments {
			m := measurements[sp.ID]
			total += m.bits
		}
		deviation := math.Abs(float64(total)-float64(plan.TargetBits)) / float64(plan.TargetBits)
		if deviation <= tolerance {
			for i := range plan.Segments {
				m := measurements[plan.Segments[i].ID]
				plan.Segments[i].PredictedBits = m.bits
				plan.Segments[i].PredictedPSNR = m.psnr
			}
			return plan, nil
		}
		if pass == 3 {
			break
		}
		reallocateByQuality(&plan, measurements)
	}
	var total uint64
	for _, sp := range plan.Segments {
		m := measurements[sp.ID]
		total += m.bits
	}
	deviation := math.Abs(float64(total)-float64(plan.TargetBits)) / float64(plan.TargetBits) * 100
	if deviation > 2.0 {
		return Plan{}, fmt.Errorf("%w: calibration deviation=%.3f%%", ErrRateDeviation, deviation)
	}
	for i := range plan.Segments {
		m := measurements[plan.Segments[i].ID]
		plan.Segments[i].PredictedBits = m.bits
		plan.Segments[i].PredictedPSNR = m.psnr
	}
	return plan, nil
}

func measurePlan(ctx context.Context, plan Plan, bundle *reel.Bundle, cfg Config) (map[int]measuredSegment, error) {
	results := make([]measuredSegment, len(plan.Segments))
	tasks := make([]task.Task, len(plan.Segments))
	for i, sp := range plan.Segments {
		i, sp := i, sp
		tasks[i] = task.Task{ID: sp.ID, Run: func(ctx context.Context, attempt int) error {
			data, err := codec.EncodeSegment(codec.SegmentMeta{ID: sp.ID, QP: sp.QP, Width: bundle.Width, Height: bundle.Height, FPS: bundle.FPS, Frames: bundle.FramesPerSeg}, bundle.Segments[sp.ID-1])
			if err != nil {
				return err
			}
			results[i] = measuredSegment{bits: data.Bits, psnr: data.PSNR}
			return nil
		}}
	}
	sched := task.Scheduler{Concurrency: cfg.Concurrency, MaxRetries: cfg.RetryMax, BackoffBase: 10 * time.Millisecond, MaxBackoff: 400 * time.Millisecond}
	if err := sched.Run(ctx, tasks); err != nil {
		return nil, err
	}
	out := make(map[int]measuredSegment, len(results))
	for i, sp := range plan.Segments {
		out[sp.ID] = results[i]
	}
	return out, nil
}

type ratePoint struct {
	qp   int
	bits uint64
}

type qualityPoint struct {
	qp   int
	psnr float64
}

func reallocateByQuality(plan *Plan, measurements map[int]measuredSegment) {
	bitSamples := make([][]ratePoint, len(plan.Segments))
	qualitySamples := make([][]qualityPoint, len(plan.Segments))
	for i, sp := range plan.Segments {
		bits := make([]ratePoint, 0, len(sp.Probes)+1)
		quality := make([]qualityPoint, 0, len(sp.Probes)+1)
		for _, probe := range sp.Probes {
			bits = append(bits, ratePoint{qp: probe.QP, bits: probe.Bits})
			quality = append(quality, qualityPoint{qp: probe.QP, psnr: probe.PSNR})
		}
		measured := measurements[sp.ID]
		bits = append(bits, ratePoint{qp: sp.QP, bits: measured.bits})
		quality = append(quality, qualityPoint{qp: sp.QP, psnr: measured.psnr})
		sort.Slice(bits, func(a, b int) bool { return bits[a].qp < bits[b].qp })
		sort.Slice(quality, func(a, b int) bool { return quality[a].qp < quality[b].qp })
		dedupBits := bits[:0]
		for _, point := range bits {
			if len(dedupBits) > 0 && dedupBits[len(dedupBits)-1].qp == point.qp {
				dedupBits[len(dedupBits)-1] = point
				continue
			}
			dedupBits = append(dedupBits, point)
		}
		dedupQuality := quality[:0]
		for _, point := range quality {
			if len(dedupQuality) > 0 && dedupQuality[len(dedupQuality)-1].qp == point.qp {
				dedupQuality[len(dedupQuality)-1] = point
				continue
			}
			dedupQuality = append(dedupQuality, point)
		}
		bitSamples[i] = dedupBits
		qualitySamples[i] = dedupQuality
	}
	target := float64(plan.TargetBits)
	bestError := math.Inf(1)
	bestQPs := make([]int, len(plan.Segments))
	low, high := 0.0, 100.0
	for iter := 0; iter < 80; iter++ {
		qualityTarget := (low + high) / 2
		qps := make([]int, len(plan.Segments))
		total := 0.0
		for i := range plan.Segments {
			qps[i] = qForPSNR(qualitySamples[i], qualityTarget, plan.MinQP, plan.MaxQP)
			total += estimateBits(bitSamples[i], qps[i])
		}
		err := math.Abs(total - target)
		if err < bestError {
			bestError = err
			copy(bestQPs, qps)
		}
		if total < target {
			low = qualityTarget
		} else {
			high = qualityTarget
		}
	}
	for i := range plan.Segments {
		plan.Segments[i].QP = bestQPs[i]
		plan.Segments[i].PredictedBits = uint64(math.Round(estimateBits(bitSamples[i], bestQPs[i])))
		plan.Segments[i].PredictedPSNR = estimatePSNR(qualitySamples[i], bestQPs[i])
	}
}

func qForPSNR(samples []qualityPoint, target float64, minQP, maxQP int) int {
	bestQP := minQP
	bestDiff := math.Inf(1)
	for qp := minQP; qp <= maxQP; qp++ {
		diff := math.Abs(estimatePSNR(samples, qp) - target)
		if diff < bestDiff {
			bestDiff = diff
			bestQP = qp
		}
	}
	return bestQP
}

func estimatePSNR(samples []qualityPoint, qp int) float64 {
	if len(samples) == 0 {
		return 0
	}
	if len(samples) == 1 || qp <= samples[0].qp {
		return interpolatePSNR(samples[0], samples[minInt(1, len(samples)-1)], qp)
	}
	last := len(samples) - 1
	if qp >= samples[last].qp {
		return interpolatePSNR(samples[last-1], samples[last], qp)
	}
	for i := 1; i < len(samples); i++ {
		if qp <= samples[i].qp {
			return interpolatePSNR(samples[i-1], samples[i], qp)
		}
	}
	return samples[last].psnr
}

func interpolatePSNR(a, b qualityPoint, qp int) float64 {
	if a.qp == b.qp {
		return a.psnr
	}
	slope := (b.psnr - a.psnr) / float64(b.qp-a.qp)
	return a.psnr + slope*float64(qp-a.qp)
}

func estimateBits(samples []ratePoint, qp int) float64 {
	if len(samples) == 0 {
		return 1
	}
	if len(samples) == 1 || qp <= samples[0].qp {
		return extrapolateBits(samples[0], samples[minInt(1, len(samples)-1)], qp)
	}
	last := len(samples) - 1
	if qp >= samples[last].qp {
		return extrapolateBits(samples[last-1], samples[last], qp)
	}
	for i := 1; i < len(samples); i++ {
		if qp <= samples[i].qp {
			return extrapolateBits(samples[i-1], samples[i], qp)
		}
	}
	return float64(samples[last].bits)
}

func extrapolateBits(a, b ratePoint, qp int) float64 {
	if a.qp == b.qp || a.bits == 0 || b.bits == 0 {
		return float64(a.bits)
	}
	slope := (math.Log2(float64(b.bits)) - math.Log2(float64(a.bits))) / float64(b.qp-a.qp)
	value := math.Exp2(math.Log2(float64(a.bits)) + slope*float64(qp-a.qp))
	if value < 1 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 1
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
