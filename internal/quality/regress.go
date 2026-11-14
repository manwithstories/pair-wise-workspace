package quality

import (
	"fmt"
	"sort"
)

type SegmentQuality struct {
	ID     int
	PSNRDB float64
}

type RegressionResult struct {
	RegressedSegments []int   `json:"regressed_segment_ids"`
	RegressedCount    int     `json:"regressed_segments"`
	MaxDropDB         float64 `json:"max_drop_db"`
	MaxDropSegment    int     `json:"max_drop_segment"`
	Verdict           string  `json:"verdict"`
}

func Compare(baseline, candidate []SegmentQuality, dropThreshold float64) (RegressionResult, error) {
	if dropThreshold < 0 {
		return RegressionResult{}, fmt.Errorf("drop threshold must be non-negative")
	}
	base := make(map[int]float64, len(baseline))
	for _, s := range baseline {
		if _, exists := base[s.ID]; exists {
			return RegressionResult{}, fmt.Errorf("duplicate baseline segment %d", s.ID)
		}
		base[s.ID] = s.PSNRDB
	}
	result := RegressionResult{RegressedSegments: []int{}, Verdict: "PASS"}
	seen := make(map[int]bool, len(candidate))
	for _, s := range candidate {
		if seen[s.ID] {
			return RegressionResult{}, fmt.Errorf("duplicate candidate segment %d", s.ID)
		}
		seen[s.ID] = true
		baselinePSNR, ok := base[s.ID]
		if !ok {
			return RegressionResult{}, fmt.Errorf("candidate segment %d has no baseline", s.ID)
		}
		drop := baselinePSNR - s.PSNRDB
		if drop > dropThreshold {
			result.RegressedSegments = append(result.RegressedSegments, s.ID)
			result.RegressedCount++
			if drop > result.MaxDropDB {
				result.MaxDropDB = drop
				result.MaxDropSegment = s.ID
			}
		}
	}
	if len(seen) != len(base) {
		return RegressionResult{}, fmt.Errorf("baseline has segments absent from candidate")
	}
	sort.Ints(result.RegressedSegments)
	if result.RegressedCount > 0 {
		result.Verdict = "REGRESSION"
	}
	return result, nil
}
