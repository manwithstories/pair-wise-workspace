package verify

import (
	"sort"

	"relayaudit/internal/clock"
	"relayaudit/internal/timeline"
)

// proxyTicks selects representative ticks for proxy rendering: one frame per
// clip start on video track 1, capped for speed.
func proxyTicks(res *timeline.Resolution) []uint64 {
	if res == nil {
		return []uint64{0}
	}
	var ticks []uint64
	for _, c := range res.Clips {
		if c.Track <= 2 {
			ticks = append(ticks, c.Start)
		}
	}
	sort.Slice(ticks, func(i, j int) bool { return ticks[i] < ticks[j] })
	if len(ticks) == 0 {
		ticks = []uint64{0, clock.Rate}
	}
	if len(ticks) > 8 {
		ticks = ticks[:8]
	}
	return ticks
}

func exitCodeFor(problems []string) int {
	if len(problems) == 0 {
		return 0
	}
	return 4
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
