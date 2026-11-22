package align

import (
	"sort"

	"mediaroute/internal/fault"
)

// MaxAlignErrorNs is the publish threshold: manifests with a larger
// cross-rendition alignment error are rejected.
const MaxAlignErrorNs = 40_000_000 // 40 ms

// Boundary records one chosen segment boundary and its alignment quality.
type Boundary struct {
	TargetNs   int64 `json:"target_ns"`
	ChosenNs   int64 `json:"chosen_ns"`
	ErrorNs    int64 `json:"error_ns"`
	Missing    bool  `json:"missing,omitempty"`
	Delayed    bool  `json:"delayed,omitempty"`
	DelayCause string `json:"delay_cause,omitempty"`
}

// Align picks one boundary per target time from the primary rendition's
// keyframes and measures the deviation of every other rendition's nearest
// keyframe. When no keyframe is available within the search window the
// boundary is delayed to the next keyframe and the cause recorded. Any
// deviation above MaxAlignErrorNs yields an align fault and the manifest
// must not be published.
func Align(targets []int64, keyframes map[string][]int64, primary string, searchWindowNs int64) ([]Boundary, []*fault.Error) {
	var out []Boundary
	var errs []*fault.Error
	for _, t := range targets {
		b := Boundary{TargetNs: t}
		chosen, ok := ceilKeyframe(keyframes[primary], t)
		if !ok || chosen-t > searchWindowNs {
			// No keyframe near the target: delay to the next one available.
			if ok {
				b.Delayed = true
				b.DelayCause = "no keyframe within search window; delayed to next keyframe"
			} else {
				errs = append(errs, fault.Newf(fault.CatAlign,
					"no keyframe at or after target %d on %s", t, primary).
					WithTrack(primary).WithRawTime(t))
				continue
			}
		}
		b.ChosenNs = chosen
		for name, ks := range keyframes {
			if name == primary {
				continue
			}
			nearest, ok := nearestKeyframe(ks, chosen)
			if !ok {
				b.Missing = true
				errs = append(errs, fault.Newf(fault.CatAlign,
					"rendition %s has no keyframe near boundary %d", name, chosen).
					WithTrack(name).WithRawTime(chosen))
				continue
			}
			if d := abs(nearest - chosen); d > b.ErrorNs {
				b.ErrorNs = d
			}
		}
		if b.ErrorNs > MaxAlignErrorNs {
			errs = append(errs, fault.Newf(fault.CatAlign,
				"alignment error %d ns exceeds %d ns at boundary %d",
				b.ErrorNs, MaxAlignErrorNs, chosen).WithRawTime(chosen))
		}
		out = append(out, b)
	}
	return out, errs
}

func ceilKeyframe(ks []int64, t int64) (int64, bool) {
	i := sort.Search(len(ks), func(i int) bool { return ks[i] >= t })
	if i >= len(ks) {
		return 0, false
	}
	return ks[i], true
}

func nearestKeyframe(ks []int64, t int64) (int64, bool) {
	if len(ks) == 0 {
		return 0, false
	}
	i := sort.Search(len(ks), func(i int) bool { return ks[i] >= t })
	if i == 0 {
		return ks[0], true
	}
	if i == len(ks) {
		return ks[len(ks)-1], true
	}
	if t-ks[i-1] <= ks[i]-t {
		return ks[i-1], true
	}
	return ks[i], true
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// P95 returns the 95th percentile of the given values (nearest-rank).
func P95(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]int64(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := (95*len(s) + 99) / 100
	return s[rank-1]
}
