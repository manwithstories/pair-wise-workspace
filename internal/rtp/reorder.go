// Package rtp 在有界窗口内对 RTP 包做重排、去重与缺口识别，
// 处理 16 位序号回绕；窗口外迟到包判定为不可恢复缺口。
package rtp

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// MaxReorderWindow 是重排窗口的硬上限（4096 包）。
const MaxReorderWindow = 4096

// Gap 描述一段连续缺失的序号区间，并携带相邻包的 RTP 时间戳，
// 便于上层映射到统一采样刻度的时间范围。
type Gap struct {
	SeqStart  uint16
	SeqEnd    uint16
	PrevTS    uint32 // 缺口前一个已收包的时间戳
	NextTS    uint32 // 缺口后第一个已收包的时间戳
	HasNext   bool
}

// ReorderBuffer 是有界重排缓冲区。零值不可用，用 NewReorderBuffer 构造。
type ReorderBuffer struct {
	window    int
	buf       map[uint16]*rtp.Packet
	base      uint16 // 下一个期望交付的序号
	haveBase  bool
	peak      int
	gaps      []Gap
	dupSame   int // 完全重复（载荷一致）计数
	dupConflict int // 同序号不同载荷（冲突损坏）计数
	late      int // 窗口外迟到计数
	// 记录最近一次推进前的序号/时间戳，用于生成缺口。
	lastSeq uint16
	lastTS  uint32
	haveLast bool
}

// NewReorderBuffer 构造窗口为 window 的重排缓冲（1..4096）。
func NewReorderBuffer(window int) (*ReorderBuffer, error) {
	if window <= 0 || window > MaxReorderWindow {
		return nil, fmt.Errorf("rtp: reorder window %d out of (0,%d]", window, MaxReorderWindow)
	}
	return &ReorderBuffer{window: window, buf: map[uint16]*rtp.Packet{}}, nil
}

// seqDelta 计算有符号 16 位序号差（b-a），与 RFC 3550 半区约定一致。
func seqDelta(a, b uint16) int32 {
	return int32(int16(b - a))
}

// 包进入窗口时的判定错误。
var (
	ErrDuplicate   = errors.New("rtp: duplicate packet with identical payload")
	ErrConflict    = errors.New("rtp: same sequence number with conflicting payload")
	ErrLateOutside = errors.New("rtp: late packet outside reorder window")
)

// Push 把包放入重排窗口。
//   - nil 错误：新包已入窗；
//   - ErrDuplicate：序号已见且载荷一致（幂等忽略）；
//   - ErrConflict：序号已见但载荷不同（标记损坏）；
//   - ErrLateOutside：包落在重排窗口之外，计为不可恢复缺口。
func (r *ReorderBuffer) Push(pkt *rtp.Packet) error {
	if pkt == nil {
		return errors.New("rtp: nil packet")
	}
	seq := pkt.SequenceNumber
	if !r.haveBase {
		r.base = seq
		r.haveBase = true
	}
	// 已缓冲的序号：区分一致重复与载荷冲突。
	if old, ok := r.buf[seq]; ok {
		if payloadEqual(old, pkt) {
			r.dupSame++
			return ErrDuplicate
		}
		r.dupConflict++
		return ErrConflict
	}
	// 不在缓冲内：相对基准判断是否窗口外迟到。
	if d := seqDelta(r.base, seq); d < 0 || d >= int32(r.window) {
		r.late++
		return ErrLateOutside
	}
	r.buf[seq] = pkt
	if n := len(r.buf); n > r.peak {
		r.peak = n
	}
	return nil
}

// Pop 按序交付下一个连续可用包；无连续包时返回 nil。
// 每当序号跨越产生缺口，返回的 gaps 在 PopAll 后通过 Gaps() 统一读取；
// 本方法在推进基准时即时登记缺口。
func (r *ReorderBuffer) Pop() *rtp.Packet {
	pkt, ok := r.buf[r.base]
	if !ok {
		return nil
	}
	delete(r.buf, r.base)
	if r.haveLast {
		if d := seqDelta(r.lastSeq, r.base); d > 1 {
			r.gaps = append(r.gaps, Gap{
				SeqStart: r.lastSeq + 1,
				SeqEnd:   r.base - 1,
				PrevTS:   r.lastTS,
				NextTS:   pkt.Timestamp,
				HasNext:  true,
			})
		}
	}
	r.lastSeq = r.base
	r.lastTS = pkt.Timestamp
	r.haveLast = true
	r.base++
	return pkt
}

// PopAll 排空当前所有可按序交付的包。
func (r *ReorderBuffer) PopAll() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		pkt := r.Pop()
		if pkt == nil {
			return out
		}
		out = append(out, pkt)
	}
}

// Flush 在流结束时把窗口内剩余包按序强制交付，并为空洞生成缺口事件。
func (r *ReorderBuffer) Flush() []*rtp.Packet {
	var out []*rtp.Packet
	for len(r.buf) > 0 {
		if pkt := r.Pop(); pkt != nil {
			out = append(out, pkt)
			continue
		}
		// 基准位置缺失：跳过一个序号并记录无右侧邻包的缺口。
		missing := r.base
		if r.haveLast {
			r.gaps = append(r.gaps, Gap{
				SeqStart: missing,
				SeqEnd:   missing,
				PrevTS:   r.lastTS,
				HasNext:  false,
			})
		}
		r.base++
	}
	return out
}

// Gaps 返回推进过程中识别到的连续缺口。
func (r *ReorderBuffer) Gaps() []Gap { return r.gaps }

// PeakBuffered 返回窗口内同时缓冲的峰值包数。
func (r *ReorderBuffer) PeakBuffered() int { return r.peak }

// Stats 返回去重、冲突与迟到计数。
func (r *ReorderBuffer) Stats() (duplicates, conflicts, late int) {
	return r.dupSame, r.dupConflict, r.late
}

func payloadEqual(a, b *rtp.Packet) bool {
	if len(a.Payload) != len(b.Payload) {
		return false
	}
	for i := range a.Payload {
		if a.Payload[i] != b.Payload[i] {
			return false
		}
	}
	return true
}
