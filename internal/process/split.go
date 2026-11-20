// Package process 提供切分、静音检测与指纹去重，全部共享统一时间基准。
package process

import "fmt"

// Segment 为一个切分窗口，[Start, End) 为主基准刻度。
type Segment struct {
	Index  int
	Start  int64
	End    int64
	Reason string // full / boundary_exact / tail_partial / tail_merged / below_min_window
}

// Split 按 window 刻度切分 [start, end)，规则确定：
//   - 总长短于 minWindow：整段输出，原因 below_min_window；
//   - 末尾余量 r==0：最后一段恰落边界，原因 boundary_exact；
//   - 0<r<minWindow：并入前一段，原因 tail_merged；
//   - r>=minWindow：独立尾段，原因 tail_partial。
func Split(start, end, window, minWindow int64) ([]Segment, error) {
	if window <= 0 || minWindow <= 0 {
		return nil, fmt.Errorf("window and minWindow must be positive, got %d/%d", window, minWindow)
	}
	if end <= start {
		return nil, fmt.Errorf("empty range [%d, %d)", start, end)
	}
	total := end - start
	if total < minWindow {
		return []Segment{{Index: 0, Start: start, End: end, Reason: "below_min_window"}}, nil
	}
	var segs []Segment
	pos := start
	idx := 0
	for pos+window <= end {
		segs = append(segs, Segment{Index: idx, Start: pos, End: pos + window, Reason: "full"})
		pos += window
		idx++
	}
	rem := end - pos
	switch {
	case rem == 0:
		segs[len(segs)-1].Reason = "boundary_exact"
	case rem < minWindow:
		segs[len(segs)-1].End = end
		segs[len(segs)-1].Reason = "tail_merged"
	default:
		segs = append(segs, Segment{Index: idx, Start: pos, End: end, Reason: "tail_partial"})
	}
	return segs, nil
}
