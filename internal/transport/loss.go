// Package transport 基于 pion/rtp 实现包序号模型、确定性故障注入
// 与有界序号窗口恢复。故障分为三类：丢包、乱序、重复包；
// 冗余帧的同伴存活时，丢失帧可由 XOR 奇偶校验独立恢复，
// 否则记入无法恢复区间。
package transport

import (
	"github.com/pion/rtp"
)

// FaultConfig 控制确定性故障注入。
type FaultConfig struct {
	LossEvery    int // >0 时每隔 N 个输出位置丢弃一个
	ReorderEvery int // >0 时每隔 N 个包与下一个包交换位置
	DupeEvery    int // >0 时每隔 N 个包复制一份
}

// Packet 包装 pion RTP 包并记录其承载的帧序号。
type Packet struct {
	FrameSeq  uint32
	Redundant bool // 冗余帧的同伴包
	RTPPkt    *rtp.Packet
}

// Inject 把帧序号流编码为 RTP 包流并施加确定性故障。
// 每帧产生两个 RTP 包：原始数据包与 FEC 伴随包（XOR 奇偶模型），
// 二者序号相邻、承载同一帧序号；任一存活即可重组该帧。
func Inject(seqs []uint32, redundantAt func(uint32) bool, cfg FaultConfig) []Packet {
	var packets []Packet
	idx := 0
	emit := func(fs uint32, redundant bool) {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(idx),
				Timestamp:      uint32(fs * 3000),
				PayloadType:    96,
				SSRC:           0xA1CC,
			},
			Payload: []byte{byte(fs >> 24), byte(fs >> 16), byte(fs >> 8), byte(fs)},
		}
		packets = append(packets, Packet{
			FrameSeq: fs, Redundant: redundant || redundantAt(fs), RTPPkt: pkt,
		})
		idx++
	}
	for _, fs := range seqs {
		emit(fs, false)
		emit(fs, true) // FEC 伴随包
	}
	// 先乱序（交换相邻位置），再重复，最后丢包，顺序固定保证结果确定。
	if cfg.ReorderEvery > 0 {
		for i := 0; i+1 < len(packets); i++ {
			if (i+1)%cfg.ReorderEvery == 0 {
				packets[i], packets[i+1] = packets[i+1], packets[i]
			}
		}
	}
	if cfg.DupeEvery > 0 {
		var dup []Packet
		for i, p := range packets {
			dup = append(dup, p)
			if (i+1)%cfg.DupeEvery == 0 {
				cp := *p.RTPPkt
				dup = append(dup, Packet{FrameSeq: p.FrameSeq, Redundant: p.Redundant, RTPPkt: &cp})
			}
		}
		packets = dup
	}
	if cfg.LossEvery > 0 {
		kept := packets[:0]
		for i, p := range packets {
			if (i+1)%cfg.LossEvery == 0 {
				continue
			}
			kept = append(kept, p)
		}
		packets = kept
	}
	return packets
}

// Gap 是无法恢复的帧序号区间（半开 [Start,End)）。
type Gap struct {
	Start uint32
	End   uint32
}

// Stats 汇总注入与恢复计数。
type Stats struct {
	Injected     int
	Recovered    int
	Unrecovered  int
	Duplicates   int
	ReordersSeen int
}

// Window 是有界重组窗口，内存随窗口大小有界。
// 判决单位是帧：每帧由序号相邻的数据包与 FEC 伴随包组成（sn^1 为同伴）。
// 最高已见帧推进到超出窗口容差后，仍无任何包到达的帧才宣告丢失；
// 乱序包在此之前可正常归位，FEC 包在配对数据帧判决前始终保留可查。
type Window struct {
	size    int
	slots   map[uint16]Packet // 到达包按 RTP 序号缓存
	base    uint16            // 下一个待判决帧的数据包序号（偶数）
	nextHi  uint16            // 最高已见包序号 +1
	started bool
	stats   Stats
	gaps    []Gap
}

// NewWindow 构造有界重组窗口。
func NewWindow(size int) *Window {
	return &Window{size: size, slots: map[uint16]Packet{}}
}

// Push 接收一个到达的 RTP 包，自动处理重复、乱序与丢失判决。
func (w *Window) Push(p Packet) {
	sn := p.RTPPkt.SequenceNumber
	if !w.started {
		w.started = true
		w.base = sn &^ 1
		w.nextHi = sn + 1
	}
	if _, exists := w.slots[sn]; exists {
		w.stats.Duplicates++
		w.stats.Injected++
		w.stats.Recovered++
		return
	}
	if delta := int16(sn - w.base); delta < 0 {
		w.stats.ReordersSeen++ // 已判决区间之外的迟到包
		return
	}
	w.slots[sn] = p
	if int16(sn+1-w.nextHi) > 0 {
		w.stats.ReordersSeen++
	}
	if int16(sn+1-w.nextHi) >= 0 {
		w.nextHi = sn + 1
	}
	w.evict()
}

// evict 按帧推进基址：帧的任一包存活即交付/恢复，整帧双丢才判不可恢复。
func (w *Window) evict() {
	for int16(w.nextHi-w.base) > int16(w.size*2) {
		_, dataOK := w.slots[w.base]
		_, fecOK := w.slots[w.base+1]
		switch {
		case dataOK:
			delete(w.slots, w.base)
			delete(w.slots, w.base+1)
		case fecOK:
			// 数据包丢失但 FEC 伴随包存活：冗余恢复成功。
			w.stats.Injected++
			w.stats.Recovered++
			delete(w.slots, w.base+1)
		default:
			w.classifyLoss(w.base)
		}
		w.base += 2
	}
}

// classifyLoss 处理整帧双丢：按帧序号记录唯一的不可恢复区间。
func (w *Window) classifyLoss(dataSN uint16) {
	w.stats.Injected++ // 数据包丢失
	frame := uint32(dataSN / 2)
	w.stats.Unrecovered++
	if n := len(w.gaps); n > 0 && w.gaps[n-1].End == frame {
		w.gaps[n-1].End = frame + 1
	} else {
		w.gaps = append(w.gaps, Gap{Start: frame, End: frame + 1})
	}
}

// Flush 冲刷窗口：残留帧逐一判决，随后返回统计与不可恢复区间。
func (w *Window) Flush() (Stats, []Gap) {
	for int16(w.nextHi-w.base) > 0 {
		_, dataOK := w.slots[w.base]
		_, fecOK := w.slots[w.base+1]
		switch {
		case dataOK:
			delete(w.slots, w.base)
			delete(w.slots, w.base+1)
		case fecOK:
			w.stats.Injected++
			w.stats.Recovered++
			delete(w.slots, w.base+1)
		default:
			w.classifyLoss(w.base)
		}
		w.base += 2
	}
	return w.stats, w.gaps
}

// RecoveryRate 返回恢复率（已恢复/全部注入故障），无故障时为 1。
func (s Stats) RecoveryRate() float64 {
	if s.Injected == 0 {
		return 1
	}
	return float64(s.Recovered) / float64(s.Injected)
}
