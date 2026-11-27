// Package fault 定义贯穿解析、时间线合并与恢复路径的统一错误模型。
// 致命错误以哨兵形式返回并决定进程退出码；非致命异常（丢包、损坏、截断）
// 收集为结构化 model.Fault，流水线继续处理可恢复片段。
package fault

import (
	"errors"
	"fmt"

	"bcast/internal/model"
)

// 故障种类。字符串值同时写入 model.Fault.Kind 与核验报告，保持稳定。
const (
	KindDrop      = "drop"      // 序号断号：丢包，保留为缺口
	KindDupe      = "dupe"      // 重复包：去重后标记
	KindCorrupt   = "corrupt"   // 帧体损坏（魔数/长度不符），截断报告字节偏移
	KindChecksum  = "checksum"  // CRC-32 校验失败
	KindTruncated = "truncated" // 素材尾部截断，最后一段不完整
	KindWrap      = "wrap"      // 无法解释的 PTS 反向跳变
	KindBoundary  = "boundary"  // 半开区间边界或跨层偏差
	KindResume    = "resume"    // 检查点/续跑不一致
)

// 哨兵错误：调用方用 errors.Is 判定，main 据此映射退出码。
var (
	// ErrInvalidInput：参数或输入无效 -> 退出码 2。
	ErrInvalidInput = errors.New("fault: invalid input")
	// ErrBoundary：边界或校验和不一致 -> 退出码 3。
	ErrBoundary = errors.New("fault: boundary mismatch")
	// ErrChecksum：核验哈希不一致 -> 退出码 3。
	ErrChecksum = errors.New("fault: checksum mismatch")
)

// Error 是带定位信息的结构化错误，可包裹哨兵错误沿调用链传递。
type Error struct {
	Code      error // 被包裹的哨兵（可为 nil）
	Kind      string
	TrackID   uint8
	Seq       uint64
	ByteStart int64
	ByteEnd   int64
	Detail    string
}

func (e *Error) Error() string {
	// 纯参数/输入类错误没有包定位信息，输出简洁信息，避免噪声字段。
	if e.TrackID == 0 && e.Seq == 0 && e.ByteStart == 0 && e.ByteEnd == 0 && e.Detail != "" {
		return e.Detail
	}
	s := fmt.Sprintf("fault %s track=%d seq=%d bytes=[%d,%d)",
		e.Kind, e.TrackID, e.Seq, e.ByteStart, e.ByteEnd)
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Code != nil {
		s += " (" + e.Code.Error() + ")"
	}
	return s
}

// Unwrap 让 errors.Is/As 能穿透到被包裹的哨兵错误。
func (e *Error) Unwrap() error { return e.Code }

// New 构造一个结构化错误。
func New(code error, kind string, track uint8, seq uint64, start, end int64, detail string) *Error {
	return &Error{Code: code, Kind: kind, TrackID: track, Seq: seq,
		ByteStart: start, ByteEnd: end, Detail: detail}
}

// InvalidInputf 包装 ErrInvalidInput 的快捷构造。
func InvalidInputf(format string, args ...any) *Error {
	return &Error{Code: ErrInvalidInput, Kind: "input", Detail: fmt.Sprintf(format, args...)}
}

// Boundaryf 包装 ErrBoundary 的快捷构造。
func Boundaryf(format string, args ...any) *Error {
	return &Error{Code: ErrBoundary, Kind: KindBoundary, Detail: fmt.Sprintf(format, args...)}
}

// Collector 确定性地收集非致命故障记录。顺序即发现顺序，
// 同输入多次运行产生完全一致的报告。
type Collector struct {
	items []model.Fault
}

// Record 追加一条故障。返回写入的记录副本便于测试断言。
func (c *Collector) Record(kind string, track uint8, seq uint64, start, end int64, detail string) model.Fault {
	f := model.Fault{Kind: kind, TrackID: track, Seq: seq,
		ByteStart: start, ByteEnd: end, Detail: detail}
	c.items = append(c.items, f)
	return f
}

// FromError 从 *Error 抽取一条记录（致命路径也需留痕时使用）。
func (c *Collector) FromError(e *Error) model.Fault {
	return c.Record(e.Kind, e.TrackID, e.Seq, e.ByteStart, e.ByteEnd, e.Detail)
}

// Items 返回收集到的全部故障（副本，外部修改不影响收集器）。
func (c *Collector) Items() []model.Fault {
	out := make([]model.Fault, len(c.items))
	copy(out, c.items)
	return out
}

// Count 返回故障总数。
func (c *Collector) Count() int { return len(c.items) }

// CountByKind 按种类统计故障数。
func (c *Collector) CountByKind(kind string) int {
	n := 0
	for _, f := range c.items {
		if f.Kind == kind {
			n++
		}
	}
	return n
}
