// Package model 的 error.go 提供可分类错误，使 CLI 能把故障映射到
// 约定的退出码（2 参数/输入无效、4 不可恢复媒体错误），并让适配器
// 与内核之间传播结构化溯源信息而非裸字符串。
package model

import (
	"errors"
	"fmt"
)

// ErrorKind 是机器可读的错误分类。
type ErrorKind string

const (
	// KindInvalidArgument 参数或用户输入无效（退出码 2）。
	KindInvalidArgument ErrorKind = "invalid_argument"
	// KindUnsupportedFormat 容器格式或编解码器版本不兼容：隔离素材（退出码 4）。
	KindUnsupportedFormat ErrorKind = "unsupported_format"
	// KindCorruptMedia 媒体数据损坏；可能保留已验证前缀（退出码 4，若有前缀则可续跑）。
	KindCorruptMedia ErrorKind = "corrupt_media"
	// KindQuotaExceeded 超过单素材大小、轨道数、解码时长或并发限制。
	KindQuotaExceeded ErrorKind = "quota_exceeded"
	// KindPathViolation 路径穿越或越权访问用户指定目录之外的路径。
	KindPathViolation ErrorKind = "path_violation"
	// KindCheckpoint 检查点状态不一致或幂等键冲突。
	KindCheckpoint ErrorKind = "checkpoint"
	// KindInterrupted 模拟中断：已提交片段完好，可续跑（退出码 3）。
	KindInterrupted ErrorKind = "interrupted"
	// KindThreshold 量化门限失败（退出码 5）。
	KindThreshold ErrorKind = "threshold"
)

// MediaError 携带分类、来源素材与（损坏场景下的）首个坏时间。
type MediaError struct {
	Kind       ErrorKind
	SourceID   string
	Message    string
	FirstBadAt MediaTime // 仅 KindCorruptMedia 有意义；零值表示未知
	Cause      error
}

// Error 实现 error。
func (e *MediaError) Error() string {
	prefix := string(e.Kind)
	if e.SourceID != "" {
		prefix += "[" + e.SourceID + "]"
	}
	if e.Kind == KindCorruptMedia && e.FirstBadAt != 0 {
		return fmt.Sprintf("%s: %s (first_bad_at=%dus)", prefix, e.Message, e.FirstBadAt.Microseconds())
	}
	return fmt.Sprintf("%s: %s", prefix, e.Message)
}

// Unwrap 支持 errors.Is/As 穿透到根因。
func (e *MediaError) Unwrap() error { return e.Cause }

// NewError 构造分类错误。
func NewError(kind ErrorKind, format string, args ...any) *MediaError {
	return &MediaError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// WrapError 包装底层错误并保留分类。
func WrapError(kind ErrorKind, cause error, format string, args ...any) *MediaError {
	return &MediaError{Kind: kind, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// WithSource 追加来源素材标识，返回同一错误以便链式调用。
func (e *MediaError) WithSource(id string) *MediaError {
	e.SourceID = id
	return e
}

// WithFirstBad 标注首个损坏时间。
func (e *MediaError) WithFirstBad(t MediaTime) *MediaError {
	e.FirstBadAt = t
	return e
}

// KindOf 提取错误分类；非 MediaError 归为 KindInvalidArgument 之外的未知类，
// 返回空字符串由调用方决定默认处理。
func KindOf(err error) ErrorKind {
	var me *MediaError
	if errors.As(err, &me) {
		return me.Kind
	}
	return ""
}

// AsMediaError 尝试提取结构化错误。
func AsMediaError(err error) (*MediaError, bool) {
	var me *MediaError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}
