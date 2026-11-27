package engine

import (
	"avrepair/internal/clock"
	"avrepair/internal/fixture"
)

// MemoryCeiling is the documented 128 MiB working-memory budget. The index
// keeps only sparse anchors in memory and streams every entry through buffered
// I/O, so live allocation stays a small constant regardless of asset length.
const MemoryCeiling = 128 << 20

// inputBytes sums the declared payload size of every generated packet. It is
// used to enforce the hard 4 GiB input ceiling before any processing starts.
func inputBytes(sc *fixture.Scenario) int64 {
	var total int64
	for _, p := range sc.Video {
		total += int64(p.Bytes)
	}
	for _, p := range sc.Audio {
		total += int64(p.Bytes)
	}
	return total
}

// checkInputSize rejects inputs at or above the 4 GiB ceiling with a structured
// CodeBadOffset error so callers map it to the argument/input failure path.
func checkInputSize(sc *fixture.Scenario) error {
	if size := inputBytes(sc); size >= clock.MaxInputBytes {
		return clock.NewError(clock.CodeBadOffset, sc.Name, clock.Interval{},
			"input exceeds the 4 GiB ceiling")
	}
	return nil
}
