// Package video 统一多机位输入的色彩元数据。
package video

import (
	"fmt"
	"strings"

	"mediatool/internal/clock"
)

// Metadata 是一组色彩描述。
type Metadata struct {
	Primaries string `json:"primaries"` // 如 bt709 / bt2020
	Transfer  string `json:"transfer"`  // 如 bt709 / pq
	Matrix    string `json:"matrix"`    // 如 bt709 / bt2020nc
	Range     string `json:"range"`     // limited / full
}

// Equal 报告两组元数据是否完全一致。
func (m Metadata) Equal(o Metadata) bool { return m == o }

// Input 是某一路输入携带的色彩元数据及其优先级（数值小者优先）。
type Input struct {
	Source   string   `json:"source"`
	Priority int      `json:"priority"`
	Meta     Metadata `json:"meta"`
}

// Record 是归一化后的规范记录：选中值、全部原始输入与选择理由都保留在清单中。
type Record struct {
	Canonical Metadata `json:"canonical"`
	Reason    string   `json:"reason"`
	Inputs    []Input  `json:"inputs"`
}

// Resolve 按输入优先级消解冲突，生成规范记录。
// 全部一致时不产生故障；存在分歧时返回已恢复的 color-conflict 故障。
func Resolve(inputs []Input) (Record, *clock.Fault, error) {
	if len(inputs) == 0 {
		return Record{}, nil, fmt.Errorf("no color metadata inputs")
	}
	rec := Record{Inputs: append([]Input(nil), inputs...)}
	best := inputs[0]
	for _, in := range inputs[1:] {
		if in.Priority < best.Priority {
			best = in
		}
	}
	rec.Canonical = best.Meta
	conflict := false
	var diffs []string
	for _, in := range inputs {
		if !in.Meta.Equal(best.Meta) {
			conflict = true
			diffs = append(diffs, fmt.Sprintf("%s=%+v", in.Source, in.Meta))
		}
	}
	if !conflict {
		rec.Reason = "all inputs agree"
		return rec, nil, nil
	}
	rec.Reason = fmt.Sprintf("priority: chose %s (priority %d) over %s",
		best.Source, best.Priority, strings.Join(diffs, "; "))
	return rec, &clock.Fault{
		Kind: clock.FaultColorConflict, Offset: clock.NoPosition, PID: clock.NoPosition,
		Recovered: true,
		Detail:    rec.Reason,
	}, nil
}
