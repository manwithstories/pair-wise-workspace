// Package ladder 负责多码率表示层的扇出独立恢复与汇合交叉核验。
// 各表示层在扇出阶段互不共享状态、可独立恢复；汇合阶段只接受
// 由共享时钟生成的清单，逐边界比对时间与关键帧对齐情况。
package ladder

import (
	"sort"

	"archivecheck/internal/model"
)

// RepResult 是单个表示层独立恢复的产物。
type RepResult struct {
	Rep      string
	Prefix   []model.Frame
	Checks   []model.CheckResult
	Manifest model.Manifest
	FirstErr *RepError
}

// RepError 以与 ingest 首错相同的口径记录表示层错误。
type RepError struct {
	Seq   uint32
	Fault model.FaultCode
}

// Fanout 按字典序确定的表示层顺序，对每层独立执行 recover 任务。
// 顺序固定保证多运行之间的结果与清单哈希确定；recover 不得跨层读写状态。
func Fanout(frames map[string][]model.Frame, recover func(rep string, fs []model.Frame) RepResult) []RepResult {
	reps := make([]string, 0, len(frames))
	for rep := range frames {
		reps = append(reps, rep)
	}
	sort.Strings(reps)
	results := make([]RepResult, 0, len(reps))
	for _, rep := range reps {
		results = append(results, recover(rep, frames[rep]))
	}
	return results
}
