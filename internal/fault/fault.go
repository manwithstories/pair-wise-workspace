// Package fault 统一记录流水线各阶段的错误：阶段、逻辑源、原始字节偏移与可恢复性。
package fault

import "fmt"

// Stage 标识错误发生的流水线阶段。
type Stage string

const (
	StageManifest  Stage = "manifest"
	StageFragment  Stage = "fragment"
	StageTransport Stage = "transport"
	StageConvert   Stage = "convert"
	StageLoudness  Stage = "loudness"
	StageValidate  Stage = "validate"
	StageCommit    Stage = "commit"
)

// Fault 是一条结构化错误记录。Source 只保存逻辑名（如清单名），
// 不保存宿主机路径，避免泄露敏感信息。
type Fault struct {
	Stage       Stage  `json:"stage"`
	Source      string `json:"source"`
	Offset      int64  `json:"offset"` // 原始字节偏移；无对应偏移时为 -1
	Recoverable bool   `json:"recoverable"`
	Message     string `json:"message"`
	Advice      string `json:"advice"` // 恢复建议
}

func (f Fault) Error() string {
	return fmt.Sprintf("[%s] %s @%d: %s", f.Stage, f.Source, f.Offset, f.Message)
}

// List 是可追加的错误集合。
type List []Fault

// Add 追加一条错误并返回更新后的列表。
func (l List) Add(f Fault) List { return append(l, f) }

// HasFatal 报告是否存在不可恢复错误。
func (l List) HasFatal() bool {
	for _, f := range l {
		if !f.Recoverable {
			return true
		}
	}
	return false
}

// FirstOffset 返回首条错误的原始偏移；无错误时返回 -1。
func (l List) FirstOffset() int64 {
	if len(l) == 0 {
		return -1
	}
	return l[0].Offset
}
