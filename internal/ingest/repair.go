package ingest

import (
	"errors"

	"archivecheck/internal/model"
)

// ErrTruncatedTail 标记尾分片未达到目标时长。
var ErrTruncatedTail = errors.New("ingest: 尾分片不完整")

// Repair 依据传输层不可恢复区间对可信前缀打标，返回逐帧校验结果。
// 可恢复帧（区间外全部帧）标记 OK；不可恢复区间内的帧标记 FaultLost，
// 不做静默填充——缺失区间显式保留，供清单与统计引用。
func Repair(prefix []model.Frame, gaps []model.Gap) []model.CheckResult {
	lost := map[uint32]bool{}
	for _, g := range gaps {
		for s := g.Start; s < g.End; s++ {
			lost[s] = true
		}
	}
	results := make([]model.CheckResult, 0, len(prefix))
	for _, f := range prefix {
		cr := model.CheckResult{Seq: f.Seq, OK: !lost[f.Seq]}
		if lost[f.Seq] {
			cr.Fault = model.FaultLost
		}
		results = append(results, cr)
	}
	return results
}

// MarkRecovered 把传输层恢复成功的帧显式标记为 Repaired，
// 与从未发生故障的帧区分开，便于恢复率审计。
func MarkRecovered(results []model.CheckResult, recoveredFrames map[uint32]bool) {
	for i := range results {
		if recoveredFrames[results[i].Seq] {
			results[i].Repaired = true
			results[i].OK = true
		}
	}
}
