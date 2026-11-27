package clock

import (
	"errors"
	"fmt"
)

// ErrCode 是跨子系统共用的稳定错误码，便于终端与测试按类别判定。
type ErrCode string

const (
	CodeInvalidHeader ErrCode = "invalid_header"   // WAV 头非法或半个采样帧
	CodeInvalidParam  ErrCode = "invalid_param"    // 参数越界/非法
	CodeOversize      ErrCode = "oversize"         // 块尺寸或内存超限
	CodePathRejected  ErrCode = "path_rejected"    // 路径越界
	CodeChecksum      ErrCode = "checksum"         // 摘要不一致
	CodeConflict      ErrCode = "conflict"         // 同序号包内容冲突
	CodeStaleIndex    ErrCode = "stale_index"      // 索引与输入不一致需重建
	CodeLatePacket    ErrCode = "late_packet"      // 窗口外迟到包
	CodeTruncated     ErrCode = "truncated"        // 截断但仍在完整采样边界
	CodeInterrupted   ErrCode = "interrupted"      // 保留检查点后主动中断
	CodeCheckpoint    ErrCode = "checkpoint"       // 检查点损坏
)

// Error 是全流水线统一错误模型，携带稳定错误码与上下文块号。
type Error struct {
	Code    ErrCode
	Message string
	Block   int64 // -1 表示不针对具体块
	Cause   error
}

func (e *Error) Error() string {
	if e.Block >= 0 {
		return fmt.Sprintf("%s: %s (block=%d)", e.Code, e.Message, e.Block)
	}
	return string(e.Code) + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError 构造带码错误，block 传 -1 表示无具体块。
func NewError(code ErrCode, block int64, format string, args ...any) *Error {
	return &Error{Code: code, Block: block, Message: fmt.Sprintf(format, args...)}
}

// WrapError 包装底层错误并保留错误码。
func WrapError(code ErrCode, block int64, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Block: block, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// CodeOf 提取错误码；非本模型错误返回空串。
func CodeOf(err error) ErrCode {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// BlockOf 提取错误关联块号；无关联返回 -1。
func BlockOf(err error) int64 {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Block
	}
	return -1
}
