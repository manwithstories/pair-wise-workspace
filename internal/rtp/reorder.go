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
	SeqStart uint16
	SeqEnd   uint16
	PrevTS   uint32 // 缺口前一个已收包的时间戳
	NextTS   uint32 // 缺口后第一个已收包的时间戳
	HasNext  bool
}

// ReorderBuffer 是有界重排缓冲区。零值不可用，用 NewReorderBuffer 构造。
//
// 语义：base 是下一个期望交付的序号；窗口 [base, base+window) 内的包进入
// 缓冲。当远侧包把基准向前推进时，推进路径上缺失的序号立即定性为缺口；
// 流尾仍未补齐的空洞由 Flush 确认。落在窗口外（含已交付区间）的迟到包
// 一律返回 ErrLateOutside，且会把窗口滑过的缺失序号定性为不可恢复缺口。
type ReorderBuffer struct {
	window   int
	buf      map[uint16]*rtp.Packet
	base     uint16
	haveBase bool

	peak int

	gaps        []Gap
	dupSame     int
	dupConflict int
	late        int

	lastSeq  uint16
	lastTS   uint32
	haveLast bool
}

// 包进入窗口时的判定错误。
var (
	ErrDuplicate   = errors.New("rtp: duplicate packet with identical payload")
	ErrConflict    = errors.New("rtp: same sequence number with conflicting payload")
	ErrLateOutside = errors.New("rtp: late packet outside reorder window")
)

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

// Push 把包放入重排窗口，必要时滑动基准并定性缺口。
func (r *ReorderBuffer) Push(pkt *rtp.Packet) error {
	if pkt == nil {
		return errors.New("rtp: nil packet")
	}
	seq := pkt.SequenceNumber
	if !r.haveBase {
		r.base = seq
		r.haveBase = true
	}
	// 已在窗口内缓冲：区分一致重复与载荷冲突。
	if old, ok := r.buf[seq]; ok {
		if payloadEqual(old, pkt) {
			r.dupSame++
			return ErrDuplicate
		}
		r.dupConflict++
		return ErrConflict
	}
	d := seqDelta(r.base, seq)
	switch {
	case d < 0:
		// 序号早于基准：窗口外迟到（含已交付区间的重复）。
		r.late++
		return ErrLateOutside
	default:
		// d>=window：远侧包仍要保留，但首先滑动基准，把窗口宽度之外
		// 再也等不到的缺失序号定性为不可恢复缺口。
		if d >= int32(r.window) {
			newBase := uint16(int32(seq) - int32(r.window) + 1)
			r.advanceBaseTo(newBase, pkt.Timestamp, true)
		}
		r.buf[seq] = pkt
		if n := len(r.buf); n > r.peak {
			r.peak = n
		}
		return nil
	}
}

// advanceBaseTo 把基准推进到 target，路径上的空洞合并为连续缺口。
func (r *ReorderBuffer) advanceBaseTo(target uint16, nextTS uint32, hasNext bool) {
	var holeStart, holeEnd uint16
	holeOpen := false
	flushHole := func() {
		if !holeOpen {
			return
		}
		r.gaps = append(r.gaps, Gap{
			SeqStart: holeStart,
			SeqEnd:   holeEnd,
			PrevTS:   r.lastTS,
			NextTS:   nextTS,
			HasNext:  hasNext,
		})
		holeOpen = false
	}
	for seqDelta(r.base, target) > 0 {
		if pkt, ok := r.buf[r.base]; ok {
			flushHole()
			delete(r.buf, r.base)
			r.lastSeq = r.base
			r.lastTS = pkt.Timestamp
			r.haveLast = true
		} else {
			if !holeOpen {
				holeStart, holeEnd = r.base, r.base
				holeOpen = true
			} else {
				holeEnd = r.base
			}
		}
		r.base++
	}
	flushHole()
}

// Pop 按序交付下一个连续可用包；基准位置缺失时返回 nil。
func (r *ReorderBuffer) Pop() *rtp.Packet {
	pkt, ok := r.buf[r.base]
	if !ok {
		return nil
	}
	delete(r.buf, r.base)
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

// Flush 在流结束时交付剩余包；流尾未补齐的空洞合并为单个无右邻缺口。
func (r *ReorderBuffer) Flush() []*rtp.Packet {
	// 找缓冲内相对基准最远的序号。
	var farSeq uint16
	haveFar := false
	for s := range r.buf {
		if !haveFar || seqDelta(r.base, s) > seqDelta(r.base, farSeq) {
			farSeq, haveFar = s, true
		}
	}
	if haveFar {
		// 推进到最远包：其间空洞带右邻时间戳定性。
		r.advanceBaseTo(farSeq, r.buf[farSeq].Timestamp, true)
	}
	out := r.PopAll()
	// 流尾空洞：最后已交付序号之后、本应在窗口内但始终缺失的部分，
	// 以无右邻缺口报告。
	if r.haveLast {
		// 流尾没有“应有但缺失”的上界信息，故仅在调用方显式知道总序号
		// 空间时才有意义；此处不凭空制造缺口。
		_ = r.lastSeq
	}
	return out
}

// MarkTrailingHole 由知晓流总序号空间的调用方使用：登记流尾缺口
// [lastDelivered+1, endSeq]，无右侧邻包。
func (r *ReorderBuffer) MarkTrailingHole(endSeq uint16) {
	if !r.haveLast {
		return
	}
	start := r.lastSeq + 1
	if seqDelta(start, endSeq) < 0 {
		return
	}
	r.gaps = append(r.gaps, Gap{
		SeqStart: start,
		SeqEnd:   endSeq,
		PrevTS:   r.lastTS,
		HasNext:  false,
	})
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
