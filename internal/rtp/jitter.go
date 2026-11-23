// Package rtp 基于统一纳秒时钟实现抖动缓冲与丢包恢复。
package rtp

import (
	"time"

	pionrtp "github.com/pion/rtp"

	"mediakernel/internal/clock"
)

// reorderWindow 是允许乱序重排的窗口（包数）。
const reorderWindow = 32

// Stats 汇总抖动缓冲的判定结果。
type Stats struct {
	Delivered int // 按序交付
	Late      int // 过期到达，计为迟到并丢弃
	Reordered int // 窗口内重排
	Dropped   int // 窗口外或重复丢弃
}

type entry struct {
	pkt     *pionrtp.Packet
	arrival clock.Nanos
}

// Buffer 是按网络采样轨迹自适应延迟上限的抖动缓冲。
type Buffer struct {
	clockRate uint32
	base      clock.Nanos
	ceiling   clock.Nanos
	maxDelay  clock.Nanos
	jitter    float64 // RFC 3550 抖动估计（纳秒）

	started   bool
	t0Arrival clock.Nanos
	t0TS      int64 // 扩展后的首个时间戳
	unw       clock.Unwrapper

	pkts    map[uint16]entry
	next    uint16 // 下一个期望输出序号
	hasNext bool
	popped  bool // 是否已输出过包

	// Gaps 记录播放时被跳过的缺失序号，交由恢复层处理。
	Gaps  []uint16
	Stats Stats
}

// NewBuffer 创建抖动缓冲；base 为基础延迟，ceiling 为自适应上限的封顶。
func NewBuffer(clockRate uint32, base, ceiling time.Duration) *Buffer {
	return &Buffer{
		clockRate: clockRate,
		base:      clock.Nanos(base),
		ceiling:   clock.Nanos(ceiling),
		maxDelay:  clock.Nanos(base),
		pkts:      make(map[uint16]entry),
	}
}

// MaxDelay 返回当前自适应延迟上限。
func (b *Buffer) MaxDelay() time.Duration { return time.Duration(b.maxDelay) }

func (b *Buffer) sentNs(extTS int64) clock.Nanos {
	return clock.TicksToNanos(extTS, int64(time.Second), int64(b.clockRate))
}

// observe 用一次传输采样更新抖动估计与延迟上限（RFC 3550 A.8）。
func (b *Buffer) observe(ts uint32, arrival clock.Nanos) {
	ext := b.unw.Extend(ts)
	if !b.started {
		b.started = true
		b.t0Arrival, b.t0TS = arrival, ext
		return
	}
	d := (arrival - b.t0Arrival) - (b.sentNs(ext) - b.sentNs(b.t0TS))
	if d < 0 {
		d = -d
	}
	b.jitter += (float64(d) - b.jitter) / 16
	md := b.base + clock.Nanos(4*b.jitter)
	if md > b.ceiling {
		md = b.ceiling
	}
	b.maxDelay = md
}

// playout 计算一个时间戳的播放时刻。
func (b *Buffer) playout(ts uint32) clock.Nanos {
	ext := b.unw.Extend(ts)
	return b.t0Arrival + (b.sentNs(ext) - b.sentNs(b.t0TS)) + b.maxDelay
}

// Push 送入一个到达包。过期包计为迟到并丢弃；
// 乱序包仅在窗口内重排；重复派发幂等。
func (b *Buffer) Push(pkt *pionrtp.Packet, arrival clock.Nanos) {
	b.observe(pkt.Timestamp, arrival)
	if b.hasNext {
		delta := int16(pkt.SequenceNumber - b.next)
		if delta < 0 && !b.popped {
			// 启动阶段尚未输出，允许更早序号成为新基准。
			b.next = pkt.SequenceNumber
			b.Stats.Reordered++
		} else {
			switch {
			case delta < 0:
				b.Stats.Dropped++ // 已输出过的旧包
				return
			case int(delta) >= reorderWindow:
				b.Stats.Dropped++
				return
			}
			if _, dup := b.pkts[pkt.SequenceNumber]; dup {
				return // 重复派发幂等
			}
			if delta > 0 {
				b.Stats.Reordered++
			}
		}
	}
	if arrival > b.playout(pkt.Timestamp) {
		b.Stats.Late++ // 过期包计为迟到并丢弃
		return
	}
	b.pkts[pkt.SequenceNumber] = entry{pkt: pkt, arrival: arrival}
	if !b.hasNext {
		b.next, b.hasNext = pkt.SequenceNumber, true
	}
}

// PopDue 释放到期的包，按序号顺序输出。
// 缺失序号在后续包已到期时跳过并记入 Gaps。
func (b *Buffer) PopDue(now clock.Nanos) []*pionrtp.Packet {
	var out []*pionrtp.Packet
	for b.hasNext {
		e, ok := b.pkts[b.next]
		if !ok {
			if !b.gapDue(now) {
				break
			}
			b.Gaps = append(b.Gaps, b.next)
			b.next++
			continue
		}
		if now < b.playout(e.pkt.Timestamp) {
			break
		}
		out = append(out, e.pkt)
		delete(b.pkts, b.next)
		b.next++
		b.popped = true
	}
	b.Stats.Delivered += len(out)
	return out
}

// gapDue 报告缺失序号之后是否已有到期包（缺失判定成立）。
func (b *Buffer) gapDue(now clock.Nanos) bool {
	for seq, e := range b.pkts {
		if int16(seq-b.next) > 0 && now >= b.playout(e.pkt.Timestamp) {
			return true
		}
	}
	return false
}
