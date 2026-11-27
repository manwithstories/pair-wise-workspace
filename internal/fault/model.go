// Package fault 定义带来源、偏移和可恢复性的错误模型，
// 供解析、恢复、合成三条流共用。
package fault

import "fmt"

// Kind 故障类别。
type Kind string

const (
	KindUnknownCodec      Kind = "unknown_codec"      // 未知编码
	KindMissingInit       Kind = "missing_init"       // 缺初始化段
	KindTruncated         Kind = "truncated"          // 尾部截断
	KindCRC               Kind = "crc_mismatch"       // 校验损坏
	KindMissingIndex      Kind = "missing_index"      // 索引缺项
	KindTimestampRollback Kind = "ts_rollback"        // 时间戳回退
	KindPacketLoss        Kind = "packet_loss"        // 丢包
	KindGap               Kind = "gap"                // 缺帧/缺块
	KindLimit             Kind = "limit_exceeded"     // 超出资源限制
	KindPathEscape        Kind = "path_escape"        // 路径逃逸
	KindTx                Kind = "tx_failure"         // 事务失败
)

// Error 带来源、偏移与可恢复性的故障。
type Error struct {
	Kind        Kind
	Source      string // 轨道或分片标识
	Offset      int64  // 字节偏移或基准 tick
	Recoverable bool
	Msg         string
}

func (e *Error) Error() string {
	return fmt.Sprintf("fault[%s] source=%s offset=%d recoverable=%t: %s",
		e.Kind, e.Source, e.Offset, e.Recoverable, e.Msg)
}

// New 构造故障。
func New(kind Kind, source string, offset int64, recoverable bool, msg string) *Error {
	return &Error{Kind: kind, Source: source, Offset: offset, Recoverable: recoverable, Msg: msg}
}

// Newf 构造带格式化消息的故障。
func Newf(kind Kind, source string, offset int64, recoverable bool, format string, args ...any) *Error {
	return &Error{Kind: kind, Source: source, Offset: offset, Recoverable: recoverable,
		Msg: fmt.Sprintf(format, args...)}
}

// First 返回偏移最小的故障（首个错误偏移），无故障返回 nil。
func First(errs []*Error) *Error {
	var best *Error
	for _, e := range errs {
		if e == nil {
			continue
		}
		if best == nil || e.Offset < best.Offset {
			best = e
		}
	}
	return best
}

// HasFatal 判断是否存在不可恢复故障。
func HasFatal(errs []*Error) bool {
	for _, e := range errs {
		if e != nil && !e.Recoverable {
			return true
		}
	}
	return false
}
