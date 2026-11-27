// Package netem 在进程内按种子确定性地模拟波动链路：
// 突发丢包、重复、乱序、持续带宽变化与停顿，全部基于虚拟纳秒时钟。
package netem

import (
	"math"
	"math/rand"

	"bcastqc/internal/media"
	rtpx "bcastqc/internal/rtp"
)

// Config 描述一条链路的损伤参数。零值表示无损伤。
type Config struct {
	Seed int64
	// Gilbert 两状态丢包：Good 状态以 LossGood 概率进入 Bad，
	// Bad 状态以 LossBad 概率丢包并以 RecoverBad 概率回到 Good。
	LossGood   float64
	LossBad    float64
	RecoverBad float64
	// Duplicate 是每个存活报文被额外复制一次的概率。
	Duplicate float64
	// Reorder 是报文被推迟到后续报文之后交付的概率。
	Reorder float64
	// BaseBPS 为基准带宽（字节/秒），<=0 表示无限带宽。
	BaseBPS int64
	// BandJitter 为带宽相对波动幅度（0~1），按正弦持续变化。
	BandJitter float64
	// OneWayDelayNS 为链路单向传播时延基线。
	OneWayDelayNS int64
	// StallStartNS/StillEndNS 为虚拟时间内的停顿区间，区间内不交付。
	StallStartNS int64
	StallEndNS   int64
}

// Event 是按交付时间排序后的链路输出。
type Event struct {
	ArriveNS  int64
	Packet    rtpx.Packet
	Duplicate bool
}

// Link 是确定性的单流链路模拟器。
type Link struct {
	cfg    Config
	rng    *rand.Rand
	bad    bool
	clock  int64 // 虚拟发送时钟
	queue  []Event
	seq    int64
}

// NewLink 基于种子创建链路。
func NewLink(cfg Config) *Link {
	return &Link{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}
}

// Accept 接收一个发送侧报文，决定其命运：
// 丢弃（返回 false）、重复或正常排入交付队列。
func (l *Link) Accept(p rtpx.Packet, sendNS int64) (delivered bool) {
	l.seq++
	if l.drop() {
		return false
	}
	arrive := l.schedule(sendNS)
	ev := Event{ArriveNS: arrive, Packet: p}
	l.queue = append(l.queue, ev)
	// 重排：与上一个已排队报文交换交付位置，并调整到达时间记账。
	if l.cfg.Reorder > 0 && l.rng.Float64() < l.cfg.Reorder && len(l.queue) >= 2 {
		last := len(l.queue) - 1
		l.queue[last-1], l.queue[last] = l.queue[last], l.queue[last-1]
		l.queue[last].ArriveNS = arrive + l.cfg.OneWayDelayNS
		l.queue[last-1].ArriveNS = arrive - 1
	}
	if l.cfg.Duplicate > 0 && l.rng.Float64() < l.cfg.Duplicate {
		dup := p
		// 重复报文稍晚到达，但沿用原始序列号与发送时间。
		l.queue = append(l.queue, Event{
			ArriveNS:  arrive + l.cfg.OneWayDelayNS/4,
			Packet:    dup,
			Duplicate: true,
		})
	}
	return true
}

// drop 用 Gilbert 模型产生成簇的突发丢包。
func (l *Link) drop() bool {
	if l.bad {
		if l.rng.Float64() < l.cfg.LossBad {
			// 仍在坏状态：丢包。
			if l.rng.Float64() < l.cfg.RecoverBad {
				l.bad = false
			}
			return true
		}
		l.bad = false
		return false
	}
	if l.cfg.LossGood > 0 && l.rng.Float64() < l.cfg.LossGood {
		l.bad = true
		// 进入坏状态的首个报文也丢弃，形成突发。
		return true
	}
	return false
}

// schedule 依据当前带宽、传播时延与停顿计算到达时间，
// 并模拟乱序：少量报文被额外延后一个抖动窗口。
func (l *Link) schedule(sendNS int64) int64 {
	arrive := sendNS + l.cfg.OneWayDelayNS
	if l.cfg.BaseBPS > 0 {
		bps := l.currentBPS(sendNS)
		if bps > 0 {
			arrive = l.clock + int64(rtpx.MTU)*media.NSPerSecond/bps
			if arrive < sendNS+l.cfg.OneWayDelayNS {
				arrive = sendNS + l.cfg.OneWayDelayNS
			}
			l.clock = arrive
		}
	}
	if l.cfg.StallEndNS > l.cfg.StallStartNS &&
		sendNS >= l.cfg.StallStartNS && sendNS < l.cfg.StallEndNS {
		// 停顿期间的报文推迟到停顿结束后交付。
		if arrive < l.cfg.StallEndNS {
			arrive = l.cfg.StallEndNS + l.cfg.OneWayDelayNS
		}
	}
	return arrive
}

// currentBPS 以确定性正弦曲线给出持续变化的瞬时带宽。
func (l *Link) currentBPS(nowNS int64) int64 {
	base := float64(l.cfg.BaseBPS)
	if l.cfg.BandJitter <= 0 {
		return l.cfg.BaseBPS
	}
	// 周期固定 250 ms，保证可复现。
	phase := float64(nowNS%250_000_000) / 250_000_000 * 2 * math.Pi
	factor := 1 - l.cfg.BandJitter + l.cfg.BandJitter*(1+math.Sin(phase))/2
	v := int64(base * factor)
	if v < 1 {
		v = 1
	}
	return v
}

// Drain 返回链路交付的报文序列。重排通过交换相邻队列位置体现，
// 因此这里保留入队（交换后）顺序；ArriveNS 仅用于时延统计。
func (l *Link) Drain() []Event {
	events := make([]Event, len(l.queue))
	copy(events, l.queue)
	l.queue = l.queue[:0]
	return events
}
