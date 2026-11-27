// Package media 同时承载跨子系统共享的错误模型。
//
// 包损坏、时间线回溯失败与音频处理失败统一为 MediaError，
// 通过 Kind 区分，通过 Unwrap 保留底层原因，便于流水线按类决策、
// 终端按类映射退出码。
package media

import (
	"errors"
	"fmt"
)

// ErrorKind 是错误类别枚举，跨包稳定，禁止重排已有值。
type ErrorKind int

const (
	KindUnknown ErrorKind = iota
	// KindPacketCorrupt 包校验失败、越界载荷、零时长等不可入列问题。
	KindPacketCorrupt
	// KindDecode 解码器拒绝输入或输出异常；携带原始包位置。
	KindDecode
	// KindTimeline 时间线推进/回溯重算失败。
	KindTimeline
	// KindAudio 重采样、声道归一或响度处理失败。
	KindAudio
	// KindCheckpoint 检查点截断、校验失败或清单不一致。
	KindCheckpoint
	// KindArgument 命令行/调用参数错误，映射退出码 2。
	KindArgument
	// KindUnrecoverable 不可恢复损坏，映射退出码 3。
	KindUnrecoverable
)

// ErrorLocation 指向坏数据在原始输入中的位置。
type ErrorLocation struct {
	StreamID uint32
	Epoch    uint32
	// Seq 是 RTP 序号（回绕前的 16 位原值）。
	Seq uint16
	// ArrivalIndex 是到达序，0 基；未知时为 -1。
	ArrivalIndex int
	// ByteOffset 是该包在输入流中的字节偏移，未知时为 -1。
	ByteOffset int64
}

// String 返回紧凑的单排定位串。
func (l ErrorLocation) String() string {
	return fmt.Sprintf("stream=%d epoch=%d seq=%d arrival=%d offset=%d",
		l.StreamID, l.Epoch, l.Seq, l.ArrivalIndex, l.ByteOffset)
}

// MediaError 是全流水线统一错误类型。
type MediaError struct {
	Kind ErrorKind
	// Op 是发生错误的操作名，如 "packet.validate"。
	Op string
	// Loc 是原始包位置；不适用时零值，ArrivalIndex/ByteOffset 为 -1。
	Loc ErrorLocation
	// Msg 是人类可读描述。
	Msg string
	// Err 是被包装的底层错误。
	Err error
}

// Error 实现 error。
func (e *MediaError) Error() string {
	s := e.Op
	if s == "" {
		s = "media"
	}
	s += ": " + e.Msg
	if e.Loc.ArrivalIndex >= 0 || e.Loc.ByteOffset >= 0 {
		s += " (" + e.Loc.String() + ")"
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap 支持 errors.Is/As 穿透。
func (e *MediaError) Unwrap() error { return e.Err }

// E 是构造 MediaError 的链式辅助器。
//
//	media.E(media.KindPacketCorrupt, "packet.validate", loc, "CRC 不匹配", err)
func E(kind ErrorKind, op string, loc ErrorLocation, msg string, cause error) *MediaError {
	return &MediaError{Kind: kind, Op: op, Loc: loc, Msg: msg, Err: cause}
}

// Wrap 在既有 MediaError 上补充操作名与信息，保留类别与位置。
func Wrap(err error, op, msg string) error {
	if err == nil {
		return nil
	}
	var me *MediaError
	if errors.As(err, &me) {
		return &MediaError{
			Kind: me.Kind,
			Op:   op,
			Loc:  me.Loc,
			Msg:  msg + ": " + me.Msg,
			Err:  me,
		}
	}
	return &MediaError{Kind: KindUnknown, Op: op, Msg: msg, Err: err}
}

// KindOf 返回错误的类别；非 MediaError 返回 KindUnknown。
func KindOf(err error) ErrorKind {
	var me *MediaError
	if errors.As(err, &me) {
		return me.Kind
	}
	return KindUnknown
}

// IsKind 判断错误是否属于给定类别。
func IsKind(err error, kind ErrorKind) bool { return KindOf(err) == kind }

// WithLocation 给错误附加原始包位置（已有位置不覆盖）。
func WithLocation(err error, loc ErrorLocation) error {
	var me *MediaError
	if errors.As(err, &me) {
		if me.Loc.ArrivalIndex < 0 && me.Loc.ByteOffset < 0 {
			me.Loc = loc
		}
		return me
	}
	return err
}
