// Package fault 定义贯穿解析、修复、索引、验证各阶段的统一错误模型。
// 每个错误都携带输入原始字节偏移、轨道、阶段与可恢复性。
package fault

import (
	"errors"
	"fmt"
)

// Stage 标识流水线阶段。
type Stage string

const (
	StageParse  Stage = "parse"
	StageRepair Stage = "repair"
	StageIndex  Stage = "index"
	StageVerify Stage = "verify"
	StageReport Stage = "report"
	StageIO     Stage = "io"
)

// Error 是带定位信息的媒体处理错误。
type Error struct {
	Stage       Stage  // 出错阶段
	TrackID     uint32 // 相关轨道，0 表示不适用
	Offset      int64  // 输入原始字节偏移，-1 表示未知
	Recoverable bool   // 是否可恢复（已验证片段可继续输出）
	Msg         string
	Err         error // 底层原因
}

func (e *Error) Error() string {
	loc := fmt.Sprintf("stage=%s offset=%d track=%d recoverable=%v",
		e.Stage, e.Offset, e.TrackID, e.Recoverable)
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", loc, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", loc, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// New 构造一个定位错误。
func New(stage Stage, trackID uint32, offset int64, recoverable bool, msg string) *Error {
	return &Error{Stage: stage, TrackID: trackID, Offset: offset, Recoverable: recoverable, Msg: msg}
}

// Wrap 包装底层错误。
func Wrap(stage Stage, trackID uint32, offset int64, recoverable bool, msg string, err error) *Error {
	return &Error{Stage: stage, TrackID: trackID, Offset: offset, Recoverable: recoverable, Msg: msg, Err: err}
}

// As 提取错误链中的 *Error。
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// Recoverable 判断错误是否可恢复；非 fault 错误一律不可恢复。
func Recoverable(err error) bool {
	if fe, ok := As(err); ok {
		return fe.Recoverable
	}
	return false
}
