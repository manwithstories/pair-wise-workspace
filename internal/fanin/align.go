// Package fanin 以共享时钟扇入各表示层并交叉校验片段边界。
package fanin

import (
	"fmt"

	"mediatool/internal/clock"
	"mediatool/internal/fanout"
)

// Result 是扇入校验结果。
type Result struct {
	Mismatches int
	FailedReps map[int]bool // 超限层单独标记失败
	Faults     clock.Faults
}

// Align 核验每层每片的实际首帧与共享时钟网格的偏差。
// 偏差超过 tolerance 的层计入 mismatches 并标记失败；其它层不受影响。
func Align(files []fanout.SegmentFile, tolerance clock.Ticks) Result {
	res := Result{FailedReps: map[int]bool{}}
	for _, f := range files {
		if !f.Complete {
			continue // 完整性由清单层判定，这里只核对边界
		}
		dev := f.FirstPTS - f.Start
		if dev < 0 {
			dev = -dev
		}
		if dev > tolerance {
			res.Mismatches++
			res.FailedReps[f.RepID] = true
			res.Faults = append(res.Faults, clock.Fault{
				Kind: clock.FaultBoundaryMismatch, Offset: clock.NoPosition, PID: clock.NoPosition,
				Detail: fmt.Sprintf("rep %d seg %d: first PTS deviates %d ticks (> %d)",
					f.RepID, f.SegID, dev, tolerance),
			})
		}
		if f.LastPTS >= f.End {
			res.Mismatches++
			res.FailedReps[f.RepID] = true
			res.Faults = append(res.Faults, clock.Fault{
				Kind: clock.FaultBoundaryMismatch, Offset: clock.NoPosition, PID: clock.NoPosition,
				Detail: fmt.Sprintf("rep %d seg %d: last PTS %d beyond segment end %d",
					f.RepID, f.SegID, f.LastPTS, f.End),
			})
		}
	}
	return res
}
