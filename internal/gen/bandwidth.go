package gen

import (
	"fmt"

	"mediascan/internal/clock"
	"mediascan/internal/rate"
)

// Bandwidth builds a deterministic piecewise-constant bandwidth trace in
// kbps that oscillates around targetKbps with congestion dips and
// recovery peaks, one sample per group of groupFrames frames.
func Bandwidth(clk clock.Clock, frames, targetKbps, groupFrames int, seed int64) ([]rate.Sample, error) {
	if frames <= 0 || targetKbps <= 0 || groupFrames <= 0 {
		return nil, fmt.Errorf("gen: invalid bandwidth params")
	}
	// deterministic wave: target * factors cycling through congestion,
	// steady and over-provisioned phases
	factors := []float64{0.5, 0.7, 1.0, 1.3, 1.1, 0.6, 0.9, 1.4}
	var out []rate.Sample
	g := 0
	for start := 0; start < frames; start += groupFrames {
		tick, err := clk.FrameTicks(int64(start))
		if err != nil {
			return nil, err
		}
		f := factors[g%len(factors)]
		// seed-dependent phase offset keeps traces distinct per seed
		phase := int(seed % 3)
		f = factors[(g+phase)%len(factors)]
		bw := int(float64(targetKbps) * f)
		if bw < 80 {
			bw = 80
		}
		out = append(out, rate.Sample{Tick: tick, Bandwidth: bw})
		g++
	}
	return out, nil
}
