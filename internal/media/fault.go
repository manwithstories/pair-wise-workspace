package media

import (
	"errors"
	"fmt"
)

// 进程退出码：0 成功；2 无法定位的损坏输入；3 不支持的参数或编码；4 写盘/检查点失败。
const (
	ExitOK          = 0
	ExitDamaged     = 2
	ExitUnsupported = 3
	ExitIO          = 4
)

// FaultKind 区分故障类别，决定统计口径与处置方式。
type FaultKind uint8

const (
	FaultNone FaultKind = iota
	// FaultTruncated 数据报长度前缀与实际字节不符或头部被截断。
	FaultTruncated
	// FaultFraming 长度前缀本身非法，无法定位下一条数据报边界（致命）。
	FaultFraming
	// FaultBadPayload 头部合法但负载无法解析（如非 L16 采样）。
	FaultBadPayload
	// FaultMissing 重排窗口滑过仍未到达的丢包。
	FaultMissing
	// FaultReordered 先缓存后按序释放的乱序包（诊断信息，非致命）。
	FaultReordered
	// FaultLate 落在窗口之外、已无法归位的迟到包。
	FaultLate
	// FaultDuplicate 重复序号包，只计数不重复输出。
	FaultDuplicate
	// FaultWrap RTP 时间戳发生 32 位回绕（诊断信息）。
	FaultWrap
)

// String 返回故障的稳定英文标识，直接用于终端键名。
func (k FaultKind) String() string {
	switch k {
	case FaultTruncated:
		return "truncated"
	case FaultFraming:
		return "framing"
	case FaultBadPayload:
		return "bad_payload"
	case FaultMissing:
		return "missing"
	case FaultReordered:
		return "reordered"
	case FaultLate:
		return "late"
	case FaultDuplicate:
		return "duplicate"
	case FaultWrap:
		return "timestamp_wrap"
	default:
		return "none"
	}
}

// Fault 携带精确定位信息：包序号、字节偏移与故障类别。
// 所有子系统通过它把时钟异常与坏包向引擎层传播。
type Fault struct {
	Kind     FaultKind `json:"kind"`
	Seq      uint16    `json:"seq"`     // RTP 序号；截断到无法读取序号时为 SeqUnknown。
	Offset   int64     `json:"offset"`  // 该数据报在输入文件中的起始字节偏移。
	Datagram int64     `json:"datagram"` // 数据报在流中的次序（0 起），与序号互为交叉验证。
	Detail   string    `json:"detail"`
}

// SeqUnknown 标记序号无法从数据报中恢复。
const SeqUnknown = ^uint16(0)

// Error 实现 error 接口。
func (f Fault) Error() string {
	seq := "?"
	if f.Seq != SeqUnknown {
		seq = fmt.Sprintf("%d", f.Seq)
	}
	return fmt.Sprintf("fault=%s seq=%s offset=%d datagram=%d %s",
		f.Kind, seq, f.Offset, f.Datagram, f.Detail)
}

// As 支持 errors.As 提取 *Fault。
func (f *Fault) As(target any) bool {
	t, ok := target.(**Fault)
	if !ok {
		return false
	}
	*t = f
	return true
}

// NewFault 构造带完整定位信息的故障。
func NewFault(kind FaultKind, seq uint16, offset, datagram int64, detail string) *Fault {
	return &Fault{Kind: kind, Seq: seq, Offset: offset, Datagram: datagram, Detail: detail}
}

// FindFault 在错误链中提取首个 *Fault。
func FindFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// UnsupportedError 标记不支持的参数或编码（退出码 3）。
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string { return "unsupported: " + e.Reason }

// IOError 包装写盘或检查点失败（退出码 4）。
type IOError struct {
	Op     string
	Path   string
	Reason string
}

func (e *IOError) Error() string {
	return fmt.Sprintf("io error op=%s path=%s: %s", e.Op, e.Path, e.Reason)
}
