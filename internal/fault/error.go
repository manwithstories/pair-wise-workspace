// Package fault 统一全流水线的错误模型：携带源块标识、字节/采样偏移
// 与可恢复性，并约定进程退出码。
package fault

import (
	"errors"
	"fmt"
)

// Kind 是错误类别，决定对外退出码与处理策略。
type Kind uint8

const (
	// KindOther 未分类的内部处理失败。
	KindOther Kind = iota
	// KindMalformedInput 输入损坏、头部非法、参数错误。
	KindMalformedInput
	// KindGap 传输缺块/窗口外迟到包等可恢复的缺口关联错误。
	KindGap
	// KindCheckpointConflict 检查点、源哈希或校验和冲突，不可自动恢复。
	KindCheckpointConflict
	// KindCancelled 上下文取消导致的停止，可从检查点续跑。
	KindCancelled
)

// Error 是所有子系统统一返回的结构化错误。
type Error struct {
	Kind         Kind
	BlockID      string // 源块标识，未知时为空
	ByteOffset   int64  // 精确字节偏移，-1 表示不适用
	SampleOffset int64  // 换算后的采样帧序号，-1 表示不适用
	Recoverable  bool   // 是否可在修复/续跑后重试
	Op           string // 发生错误的操作
	Message      string
	Err          error
}

// New 构造一个结构化错误。
func New(kind Kind, blockID string, byteOffset, sampleOffset int64, recoverable bool, message string) *Error {
	return &Error{
		Kind:         kind,
		BlockID:      blockID,
		ByteOffset:   byteOffset,
		SampleOffset: sampleOffset,
		Recoverable:  recoverable,
		Message:      message,
	}
}

// Wrap 在保留结构化字段的同时包装底层错误。
func (e *Error) Wrap(err error) *Error {
	cp := *e
	cp.Err = err
	return &cp
}

// WithOp 标注发生错误的操作名。
func (e *Error) WithOp(op string) *Error {
	cp := *e
	cp.Op = op
	return &cp
}

// Error 实现 error 接口，输出可复核的完整上下文。
func (e *Error) Error() string {
	s := e.Message
	if e.Op != "" {
		s = e.Op + ": " + s
	}
	if e.BlockID != "" {
		s += fmt.Sprintf(" [block=%s]", e.BlockID)
	}
	if e.ByteOffset >= 0 {
		s += fmt.Sprintf(" [byte=%d]", e.ByteOffset)
	}
	if e.SampleOffset >= 0 {
		s += fmt.Sprintf(" [sample=%d]", e.SampleOffset)
	}
	s += fmt.Sprintf(" [recoverable=%t]", e.Recoverable)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap 支持 errors.Is/As 穿透底层错误。
func (e *Error) Unwrap() error { return e.Err }

// As 从任意错误中提取结构化错误。
func As(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// 退出码约定：0 全部通过；2 输入损坏或参数错误；
// 3 不可恢复的检查点/校验和冲突；1 内部处理失败。
const (
	ExitOK              = 0
	ExitInternal        = 1
	ExitInput           = 2
	ExitCheckpointFault = 3
)

// ExitCode 按错误类别映射进程退出码。
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	fe, ok := As(err)
	if !ok {
		return ExitInternal
	}
	switch fe.Kind {
	case KindMalformedInput, KindGap:
		return ExitInput
	case KindCheckpointConflict:
		return ExitCheckpointFault
	case KindCancelled:
		// 取消本身是可恢复停机，交由调用方决定；默认按内部失败处理。
		return ExitInternal
	default:
		return ExitInternal
	}
}
