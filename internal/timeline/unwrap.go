package timeline

import (
	"sort"

	"mp4probe/internal/box"
	"mp4probe/internal/moov"
)

// wrapMod is the 32-bit timestamp modulus.
const wrapMod = uint64(1) << 32

// ExpandDTS accumulates stts deltas into 64-bit decode timestamps.
// Hardware encoders write 32-bit timestamps; whenever the cumulative
// value crosses a 2^32 boundary a wrap event is counted and the
// generation is folded into the 64-bit result, so DTS stays monotonic.
// The stts run lengths must sum to sampleCount exactly.
func ExpandDTS(stts []moov.SttsEntry, sampleCount int) ([]int64, int, error) {
	var total uint64
	for _, e := range stts {
		total += uint64(e.Count)
	}
	if total != uint64(sampleCount) {
		return nil, 0, box.Errorf("expand dts", 0,
			"stts sample total %d != stsz sample count %d", total, sampleCount)
	}
	dts := make([]int64, sampleCount)
	var cum uint64
	var idx int
	wraps := 0
	prevGen := uint64(0)
	for _, e := range stts {
		for i := uint32(0); i < e.Count; i++ {
			dts[idx] = int64(cum)
			idx++
			cum += uint64(e.Delta)
			if gen := cum / wrapMod; gen != prevGen {
				wraps++
				prevGen = gen
			}
		}
	}
	return dts, wraps, nil
}

// ComputePTS derives presentation timestamps from DTS and the ctts
// composition offsets. With no ctts, PTS equals DTS. Offsets apply in
// the unwrapped 64-bit domain so B-frame reordering near a 2^32 wrap
// does not misplace frames.
func ComputePTS(dts []int64, ctts []moov.CttsEntry) ([]int64, error) {
	pts := make([]int64, len(dts))
	if len(ctts) == 0 {
		copy(pts, dts)
		return pts, nil
	}
	var total uint64
	for _, e := range ctts {
		total += uint64(e.Count)
	}
	if total != uint64(len(dts)) {
		return nil, box.Errorf("compute pts", 0,
			"ctts sample total %d != sample count %d", total, len(dts))
	}
	var idx int
	for _, e := range ctts {
		for i := uint32(0); i < e.Count; i++ {
			pts[idx] = dts[idx] + int64(e.Offset)
			idx++
		}
	}
	return pts, nil
}

// PresentationOrder returns sample indices sorted by PTS (stable), the
// order frames are displayed in. Seek uses it to binary-search a PTS
// target without tripping over B-frame reordering.
func PresentationOrder(pts []int64) []int {
	order := make([]int, len(pts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return pts[order[a]] < pts[order[b]]
	})
	return order
}
