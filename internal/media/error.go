package media

import (
	"errors"
	"fmt"
)

// Stage 标识异常发生的管线阶段。
type Stage string

// 管线各阶段标识，错误模型由各模块共用。
const (
	StageParse    Stage = "parse"    // EBML/Matroska 解读
	StageTransport Stage = "transport" // RTP 模拟传输
	StageRecover  Stage = "recover"  // 抖动缓冲与重传恢复
	StageSync     Stage = "sync"     // 音画对齐
	StageMux      Stage = "mux"      // 再封装
	StageCLI      Stage = "cli"      // 命令适配
)

// Kind 区分错误类别，决定退出码与是否可继续批次。
type Kind string

// 错误类别。
const (
	KindTruncated  Kind = "truncated"  // 元素或头部被截断
	KindBoundary   Kind = "boundary"   // 越过父元素边界
	KindTimestamp  Kind = "timestamp"  // 负时间戳 / 时间戳回退
	KindOverrun    Kind = "overrun"    // 队列或缓存超出字节上限
	KindGap        Kind = "gap"        // 重传窗口外的不可恢复缺口
	KindSync       Kind = "sync"       // 音画偏差超阈
	KindCanceled   Kind = "canceled"   // 上下文取消
	KindArg        Kind = "arg"        // 参数或输入错误
	KindIO         Kind = "io"         // I/O 或意外不完整
)

// Error 是全管线统一的带上下文错误模型。
type Error struct {
	Stage       Stage
	Kind        Kind
	Track       uint32 // 0 表示不隶属于具体轨道
	Offset      int64  // 首个缺失/越界字节偏移；未知为 -1
	Recoverable bool   // 是否可经重传等手段恢复
	Detail      string
	Err         error
}

// NewError 创建管线错误，偏移未知时取 -1。
func NewError(stage Stage, kind Kind, detail string) *Error {
	return &Error{Stage: stage, Kind: kind, Track: TrackNone, Offset: -1, Detail: detail}
}

// WithTrack 绑定轨道号并返回错误本身。
func (e *Error) WithTrack(track uint32) *Error { e.Track = track; return e }

// WithOffset 绑定字节偏移并返回错误本身。
func (e *Error) WithOffset(off int64) *Error { e.Offset = off; return e }

// MarkRecoverable 标记为可恢复并返回错误本身。
func (e *Error) MarkRecoverable() *Error { e.Recoverable = true; return e }

// Wrap 包装底层错误并返回错误本身。
func (e *Error) Wrap(err error) *Error { e.Err = err; return e }

// Error 实现 error 接口。
func (e *Error) Error() string {
	s := string(e.Stage) + "/" + string(e.Kind)
	if e.Track != TrackNone {
		s += fmt.Sprintf(" track=%d", e.Track)
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.Recoverable {
		s += " [recoverable]"
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap 支持 errors.Is/As 穿透。
func (e *Error) Unwrap() error { return e.Err }

// AsError 把任意错误规整为 *media.Error；已是则直接返回，
// 否则按默认阶段/类别包装。
func AsError(err error, defaultStage Stage, defaultKind Kind) *Error {
	if err == nil {
		return nil
	}
	var me *Error
	if errors.As(err, &me) {
		return me
	}
	return &Error{Stage: defaultStage, Kind: defaultKind, Offset: -1, Detail: err.Error(), Err: err}
}

// IsKind 判断错误链上是否存在指定 Kind 的 *Error。
func IsKind(err error, kind Kind) bool {
	var me *Error
	if errors.As(err, &me) {
		return me.Kind == kind
	}
	return false
}
