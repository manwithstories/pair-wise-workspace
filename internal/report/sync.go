package report

import (
	"math"

	"archive/internal/clock"
)

// AVSyncErrors pairs each video presentation time with the nearest audio
// presentation time and returns the absolute error per pair, in
// milliseconds. Inputs are PTS values on the common timebase, ascending.
func AVSyncErrors(videoPTS, audioPTS []int64) []float64 {
	if len(videoPTS) == 0 || len(audioPTS) == 0 {
		return nil
	}
	errs := make([]float64, 0, len(videoPTS))
	j := 0
	for _, v := range videoPTS {
		for j+1 < len(audioPTS) && absDiff(audioPTS[j+1], v) <= absDiff(audioPTS[j], v) {
			j++
		}
		errs = append(errs, math.Abs(clock.ToMilliseconds(v-audioPTS[j])))
	}
	return errs
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}
