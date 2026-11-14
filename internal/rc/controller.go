// Package rc assigns per-frame QPs to hit a segment bit target.
package rc

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/stat"
	"tcr/internal/codec"
	"tcr/internal/yuv"
)

// tolerance is the accepted relative deviation from the segment target.
const tolerance = 0.025

// InfeasibleError reports a target below the achievable bit floor.
type InfeasibleError struct {
	SegIndex   int
	TargetBits int64
	MinBits    int64
	MinKbps    float64
}

func (e *InfeasibleError) Error() string {
	return fmt.Sprintf("infeasible: segment %d needs at least %d bits (%.1f kbps at this duration), target is %d bits",
		e.SegIndex, e.MinBits, e.MinKbps, e.TargetBits)
}

// Outcome describes the encoded segment.
type Outcome struct {
	QPEff float64
	Bits  int64
}

// EncodeSegment encodes frames to outPath, targeting targetBits within tolerance.
func EncodeSegment(frames []*yuv.Frame, hdr codec.SegmentHeader, targetBits int64, outPath string) (*Outcome, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("segment %d: zero frames", hdr.SegIndex)
	}
	if targetBits <= 0 {
		return nil, fmt.Errorf("segment %d: target bits must be positive, got %d", hdr.SegIndex, targetBits)
	}
	uniform := func(qp int) []int {
		qps := make([]int, len(frames))
		for i := range qps {
			qps[i] = qp
		}
		return qps
	}
	bitsAt := func(qps []int) int64 { return codec.MeasureBits(frames, qps, hdr) }

	minBits := bitsAt(uniform(codec.QPMax))
	if targetBits < minBits {
		secs := float64(len(frames)) / float64(hdr.FPS)
		return nil, &InfeasibleError{
			SegIndex:   hdr.SegIndex,
			TargetBits: targetBits,
			MinBits:    minBits,
			MinKbps:    float64(minBits) / secs / 1000,
		}
	}

	// Probe the bits-QP curve and fit log2(bits) = alpha + beta*QP.
	probes := []int{8, 24, 40}
	xs := make([]float64, 0, 4)
	ys := make([]float64, 0, 4)
	for _, qp := range probes {
		xs = append(xs, float64(qp))
		ys = append(ys, math.Log2(float64(bitsAt(uniform(qp)))))
	}
	alpha, beta := stat.LinearRegression(xs, ys, nil, false)
	logTarget := math.Log2(float64(targetBits))
	qpEff := (logTarget - alpha) / beta
	if math.IsNaN(qpEff) || qpEff < codec.QPMin {
		qpEff = codec.QPMin
	}
	if qpEff > codec.QPMax {
		qpEff = codec.QPMax
	}

	// Refine with measured feedback: secant interpolation between
	// samples bracketing the target, falling back to the fitted slope.
	n := len(frames)
	type sample struct {
		qpEff float64
		q, hi int
		bits  int64
	}
	var samples []sample
	seen := map[int]bool{}
	split := func(qpEff float64) (int, int) {
		if qpEff < codec.QPMin {
			qpEff = codec.QPMin
		}
		if qpEff > codec.QPMax {
			qpEff = codec.QPMax
		}
		q := int(math.Floor(qpEff))
		hi := 0
		if q < codec.QPMax {
			hi = int(math.Round((qpEff - float64(q)) * float64(n)))
			if hi > n {
				hi = n
			}
		}
		return q, hi
	}
	best := sample{bits: -1}
	for iter := 0; iter < 12; iter++ {
		q, hi := split(qpEff)
		key := q*(n+1) + hi
		if seen[key] {
			// Stall: step to the nearest unmeasured neighbor toward target.
			if best.bits > targetBits {
				if hi < n {
					hi++
				} else {
					q++
				}
			} else {
				if hi > 0 {
					hi--
				} else {
					q--
				}
			}
			if q < codec.QPMin {
				q = codec.QPMin
			}
			if q > codec.QPMax {
				q = codec.QPMax
			}
			key = q*(n+1) + hi
			if seen[key] {
				break // neighborhood exhausted
			}
		}
		seen[key] = true
		qps := mixQPs(n, q, hi)
		bits := bitsAt(qps)
		eff := float64(q)
		if q < codec.QPMax {
			eff += float64(hi) / float64(n)
		}
		s := sample{qpEff: eff, q: q, hi: hi, bits: bits}
		samples = append(samples, s)
		if best.bits < 0 || absDev(bits, targetBits) < absDev(best.bits, targetBits) {
			best = s
		}
		if absDev(bits, targetBits) <= tolerance {
			best = s
			break
		}
		// Find the tightest measured bracket around the target.
		var lo, hiS *sample // lo: bits > target, hiS: bits < target
		for i := range samples {
			smp := &samples[i]
			if smp.bits > targetBits && (lo == nil || smp.bits < lo.bits) {
				lo = smp
			}
			if smp.bits < targetBits && (hiS == nil || smp.bits > hiS.bits) {
				hiS = smp
			}
		}
		switch {
		case lo != nil && hiS != nil:
			// Secant in (qpEff, log2 bits) space.
			l1, l2 := math.Log2(float64(lo.bits)), math.Log2(float64(hiS.bits))
			qpEff = lo.qpEff + (logTarget-l1)*(hiS.qpEff-lo.qpEff)/(l2-l1)
		case lo != nil:
			qpEff = lo.qpEff + (math.Log2(float64(lo.bits))-logTarget)/(-beta)
		default:
			qpEff = hiS.qpEff - (logTarget-math.Log2(float64(hiS.bits)))/(-beta)
		}
	}

	qps := mixQPs(n, best.q, best.hi)
	qpEff = best.qpEff
	written, err := codec.WriteSegmentFile(outPath, hdr, frames, qps)
	if err != nil {
		return nil, err
	}
	return &Outcome{QPEff: qpEff, Bits: written * 8}, nil
}

func absDev(bits, target int64) float64 {
	d := float64(bits-target) / float64(target)
	if d < 0 {
		return -d
	}
	return d
}

// mixQPs builds a per-frame QP list whose average equals qpEff by
// interleaving floor and ceil QPs with a Bresenham-style spread.
func mixQPs(n, q, hiCount int) []int {
	hi := q + 1
	if hi > codec.QPMax {
		hi = codec.QPMax
	}
	qps := make([]int, n)
	acc := 0.0
	for i := 0; i < n; i++ {
		acc += float64(hiCount) / float64(n)
		if acc >= 1 {
			qps[i] = hi
			acc--
		} else {
			qps[i] = q
		}
	}
	return qps
}
