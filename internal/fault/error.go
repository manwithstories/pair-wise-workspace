// Package fault 定义流水线统一的错误分类。
//
// 错误分为两大类：
//   - 可恢复错误（截断、损坏样本、索引缺页等）：只标记受影响区间，
//     流水线必须跳过该区间继续处理其余素材；
//   - 致命错误（参数非法、路径逃逸、片段超限等）：本次运行无法继续。
package fault

import (
	"errors"
	"fmt"
)

// Kind 表示错误的可恢复性分类。
type Kind int

const (
	// KindUnknown 是未分类错误的零值，按致命处理。
	KindUnknown Kind = iota
	// KindRecoverable 可恢复：局部故障，不得扩散到整条轨道。
	KindRecoverable
	// KindFatal 致命：输入或参数本身无效，必须终止本次运行。
	KindFatal
)

// 稳定的错误码，便于测试与终端输出按码断言。
const (
	CodeTruncated     = "truncated"      // 盒或文件在声明长度前结束
	CodeCorrupt       = "corrupt_sample" // 样本盒内容损坏
	CodeIndexMissing  = "index_missing"  // 关键帧索引缺页
	CodeIncomplete    = "incomplete"     // 剪辑点落在损坏/截断区间
	CodeCacheTampered = "cache_tampered" // 缓存内容与摘要不符
	CodeTooLarge      = "segment_too_large"
	CodePathEscape    = "path_escape"
	CodeInvalidInput  = "invalid_input"
)

// Error 是带分类、错误码与受影响区间的流水线错误。
type Error struct {
	Kind Kind
	Code string
	Op   string // 产生错误的操作，例如 "mp4.ReadBox"
	// AffectedStart/AffectedEnd 以轨道时基 tick 表示受影响区间，
	// 未知时两端相等（零长度区间）。
	AffectedStart int64
	AffectedEnd   int64
	Msg           string
	Err           error
}

func (e *Error) Error() string {
	scope := ""
	if e.AffectedEnd > e.AffectedStart {
		scope = fmt.Sprintf(" range=[%d,%d)", e.AffectedStart, e.AffectedEnd)
	}
	op := ""
	if e.Op != "" {
		op = e.Op + ": "
	}
	if e.Err != nil {
		return fmt.Sprintf("fault(%s/%s)%s %s%v", kindName(e.Kind), e.Code, scope, op, e.Err)
	}
	return fmt.Sprintf("fault(%s/%s)%s %s%s", kindName(e.Kind), e.Code, scope, op, e.Msg)
}

// Unwrap 支持 errors.Is/As 穿透到底层错误。
func (e *Error) Unwrap() error { return e.Err }

func kindName(k Kind) string {
	switch k {
	case KindRecoverable:
		return "recoverable"
	case KindFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// New 构造一个不带底层错误的分类错误。
func New(kind Kind, code, msg string) *Error {
	return &Error{Kind: kind, Code: code, Msg: msg}
}

// Recoverable 用可恢复分类包装底层错误，并记录受影响区间。
func Recoverable(code, op string, start, end int64, err error) *Error {
	return &Error{
		Kind:          KindRecoverable,
		Code:          code,
		Op:            op,
		AffectedStart: start,
		AffectedEnd:   end,
		Err:           err,
	}
}

// Fatal 用致命分类包装底层错误。
func Fatal(code, op string, err error) *Error {
	return &Error{Kind: KindFatal, Code: code, Op: op, Err: err}
}

// WithRange 复制错误并附加受影响区间，保持其余字段不变。
func WithRange(e *Error, start, end int64) *Error {
	cp := *e
	cp.AffectedStart, cp.AffectedEnd = start, end
	return &cp
}

// IsRecoverable 判断错误链中是否存在可恢复分类错误。
func IsRecoverable(err error) bool {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Kind == KindRecoverable
	}
	return false
}

// IsFatal 判断错误链中是否存在致命分类错误。
func IsFatal(err error) bool {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Kind == KindFatal
	}
	return false
}

// CodeOf 返回错误链中第一个分类错误的错误码，未分类时返回空串。
func CodeOf(err error) string {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}
