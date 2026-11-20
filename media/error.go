package media

import (
	"errors"
	"fmt"
)

// Stage 标识流水线中产生错误的阶段。
type Stage string

// 流水线各阶段。
const (
	StageParse     Stage = "parse"
	StageTimecode  Stage = "timecode"
	StageTransport Stage = "transport"
	StageTransform Stage = "transform"
	StageSync      Stage = "sync"
	StageValidate  Stage = "validate"
	StageManifest  Stage = "manifest"
)

// Error 是带阶段、轨道、原始字节偏移与可恢复性的错误模型。
// Offset 指向原始输入中出错记录的首字节，使恢复结果可追溯。
type Error struct {
	Stage       Stage
	Track       string // 轨道/流标识，可为空
	Offset      int64  // 原始输入字节偏移，-1 表示不适用
	Recoverable bool   // true 时流水线跳过该记录继续处理后续有效记录
	Msg         string
}

func (e *Error) Error() string {
	return fmt.Sprintf("[%s] track=%q offset=%d recoverable=%t: %s",
		e.Stage, e.Track, e.Offset, e.Recoverable, e.Msg)
}

// New 按 printf 风格构造错误；offset 不适用时传 -1。
func New(stage Stage, track string, offset int64, recoverable bool, format string, args ...any) *Error {
	return &Error{
		Stage:       stage,
		Track:       track,
		Offset:      offset,
		Recoverable: recoverable,
		Msg:         fmt.Sprintf(format, args...),
	}
}

// As 把任意 error 转为 *Error；非本模型错误包装为不可恢复错误。
func As(err error) *Error {
	var me *Error
	if errors.As(err, &me) {
		return me
	}
	return &Error{Stage: StageTransform, Offset: -1, Msg: err.Error()}
}

// IsRecoverable 报告 err 是否为可恢复错误（nil 视为可恢复）。
func IsRecoverable(err error) bool {
	if err == nil {
		return true
	}
	return As(err).Recoverable
}
