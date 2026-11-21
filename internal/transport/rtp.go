// Package transport 按可配置窗口重排 RTP 包并统计丢包恢复，
// 通过共享有理数时钟把包时间映射到分片区间。
package transport

import (
	"github.com/pion/rtp"

	"mediadeliver/internal/clock"
	"mediadeliver/internal/fault"
)

// MaxWindow 是允许的最大重排窗口（内存有界）。
const MaxWindow = 512

// Stats 统计接收与恢复情况。
type Stats struct {
	Expected   int `json:"expected"`   // 序号空间 [first,hi] 内应有的包数
	Received   int `json:"received"`   // 去重后实际收到的包数
	Duplicates int `json:"duplicates"` // 重复包（不重复计数）
	Recovered  int `json:"recovered"`  // 窗口内乱序但成功恢复的包数
	Lost       int `json:"lost"`       // 窗口外丢包（不可恢复）
}

// RecoveryRate 返回丢包恢复率：恢复数 / (恢复数 + 丢失数)。
func (s Stats) RecoveryRate() float64 {
	d := s.Recovered + s.Lost
	if d == 0 {
		return 1
	}
	return float64(s.Recovered) / float64(d)
}

// Reorderer 是固定窗口的 RTP 重排器。
type Reorderer struct {
	window  int
	base    uint16 // 下一个期望序号
	hi      uint16 // 已见最大序号
	started bool
	buf     map[uint16]*rtp.Packet
	stats   Stats
	Faults  fault.List
}

// NewReorderer 创建重排器；窗口非法或超限返回 nil。
func NewReorderer(window int) *Reorderer {
	if window <= 0 || window > MaxWindow {
		return nil
	}
	return &Reorderer{window: window, buf: make(map[uint16]*rtp.Packet)}
}

// Stats 返回当前统计。
func (r *Reorderer) Stats() Stats { return r.stats }

// Push 输入一个原始 RTP 报文，返回本次可有序交付的包。
func (r *Reorderer) Push(raw []byte) []*rtp.Packet {
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		r.Faults = r.Faults.Add(fault.Fault{Stage: fault.StageTransport, Source: "rtp",
			Offset: -1, Recoverable: true, Message: "malformed RTP packet: " + err.Error(),
			Advice: "丢弃该报文继续接收"})
		return nil
	}
	seq := pkt.SequenceNumber
	if !r.started {
		r.base, r.hi, r.started = seq, seq, true
		r.stats.Expected = 1
	} else if d := int16(seq - r.hi); d > 0 {
		r.stats.Expected += int(d)
		r.hi = seq
	}
	diff := int16(seq - r.base)
	switch {
	case diff < 0:
		r.stats.Duplicates++ // 已交付序号的重发，不重复计数
	case diff == 0:
		r.stats.Received++
		r.base++
		return append([]*rtp.Packet{pkt}, r.drain()...)
	case int(diff) <= r.window:
		if _, ok := r.buf[seq]; ok {
			r.stats.Duplicates++
			return nil
		}
		r.buf[seq] = pkt
		r.stats.Received++
	default:
		out := r.advanceTo(seq) // 窗口外：空槽记不可恢复丢包
		r.stats.Received++
		r.base++
		out = append(out, pkt)
		return append(out, r.drain()...)
	}
	return nil
}

// Flush 在流结束时推进到 hi，交付残余缓冲并把空缺记为丢包。
func (r *Reorderer) Flush() []*rtp.Packet {
	if !r.started {
		return nil
	}
	if int16(r.hi-r.base) < 0 {
		return nil // base 已越过 hi，无残余
	}
	out := r.advanceTo(r.hi)
	if pkt, ok := r.buf[r.base]; ok { // hi 本身可能在缓冲中
		delete(r.buf, r.base)
		r.stats.Recovered++
		out = append(out, pkt)
	}
	r.base = r.hi + 1
	return out
}

// advanceTo 把 base 推进到 target：途中缓冲槽交付（计恢复），空槽记丢包。
func (r *Reorderer) advanceTo(target uint16) []*rtp.Packet {
	var out []*rtp.Packet
	lost := 0
	for r.base != target {
		if pkt, ok := r.buf[r.base]; ok {
			delete(r.buf, r.base)
			r.stats.Recovered++
			out = append(out, pkt)
		} else {
			lost++
		}
		r.base++
	}
	if lost > 0 {
		r.stats.Lost += lost
		r.Faults = r.Faults.Add(fault.Fault{Stage: fault.StageTransport, Source: "rtp",
			Offset: -1, Message: "packet gap beyond reorder window",
			Advice: "标记该区间为丢包，后续分片继续"})
	}
	return out
}

// drain 交付缓冲中从 base 起连续的包。
func (r *Reorderer) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		pkt, ok := r.buf[r.base]
		if !ok {
			return out
		}
		delete(r.buf, r.base)
		r.stats.Recovered++
		out = append(out, pkt)
		r.base++
	}
}

// PacketTime 将 RTP 时间戳映射到统一时间基准。
func PacketTime(ts uint32, clockRate int64) clock.Rational {
	return clock.New(int64(ts), clockRate)
}
