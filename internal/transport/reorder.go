// Package transport 使用 pion/rtp 对 RTP 输入去重、重排并处理丢失，
// 输出按媒体时间排序的媒体单元；缺口显式填充静音，绝不静默压缩时间线。
package transport

import (
	"github.com/pion/rtp"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

// GapPolicy 突发丢失的处理策略。
type GapPolicy int

const (
	PolicyFillSilence GapPolicy = iota // 填充静音，保持时间线
	PolicyStop                         // 停止并报错
)

// Unit 是有序输出单元：媒体包或显式静音填充。
type Unit struct {
	Silence  bool
	Packet   *rtp.Packet // Silence=false 时有效
	Start    clock.Ticks // 统一时钟上的起点
	Duration clock.Ticks
}

// Stats 传输统计。
type Stats struct {
	DuplicatePackets int `json:"duplicate_packets"`
	ReorderedPackets int `json:"reordered_packets"`
	GapEvents        int `json:"gap_events"`
	GapPackets       int `json:"gap_packets"`
}

// Reorderer 按序号与时间戳恢复有序流。
type Reorderer struct {
	Window           int // 乱序窗口（包数），窗口外报错
	ClockRate        int // RTP 时钟频率
	SamplesPerPacket int // 每包采样数（用于静音时长）
	Policy           GapPolicy

	Stats   Stats
	head    uint16 // 下一个应发出的序号
	started bool
	buf     map[uint16]*rtp.Packet
	lastTS  uint32
	hasTS   bool
	lastEnd clock.Ticks // 上一个发出单元的终点（统一时钟）
}

// New 创建重排器。
func New(window, clockRate, samplesPerPacket int, policy GapPolicy) *Reorderer {
	return &Reorderer{Window: window, ClockRate: clockRate,
		SamplesPerPacket: samplesPerPacket, Policy: policy, buf: map[uint16]*rtp.Packet{}}
}

func (r *Reorderer) pktDur() clock.Ticks {
	return clock.SamplesToTicks(int64(r.SamplesPerPacket), r.ClockRate)
}

// Feed 喂入一个 RTP 包，返回本次可发出的有序单元。
func (r *Reorderer) Feed(pkt *rtp.Packet) ([]Unit, error) {
	if !r.started {
		r.head = pkt.SequenceNumber
		r.started = true
	}
	// 重复：已发出（序号落后于 head 且不在缓冲）或已在缓冲。
	if _, dup := r.buf[pkt.SequenceNumber]; dup || (r.started && seqLess(pkt.SequenceNumber, r.head)) {
		r.Stats.DuplicatePackets++
		return nil, nil
	}
	// 乱序：超前于 head 到达，或迟到（缓冲中已有更靠后的包）。
	if pkt.SequenceNumber != r.head || len(r.buf) > 0 {
		r.Stats.ReorderedPackets++
	}
	r.buf[pkt.SequenceNumber] = pkt
	var out []Unit
	for {
		p, ok := r.buf[r.head]
		if ok {
			delete(r.buf, r.head)
			u, err := r.emit(p)
			if err != nil {
				return out, err
			}
			out = append(out, u)
			r.head++
			continue
		}
		if len(r.buf) > r.Window {
			// 窗口外：判定 head 丢失
			r.Stats.GapEvents++
			r.Stats.GapPackets++
			if r.Policy == PolicyStop {
				return out, fault.New(fault.KindTransport, fault.Fail, "rtp", 0, int(r.head),
					"packet lost beyond reorder window (burst loss)")
			}
			out = append(out, r.silenceUnit())
			r.head++
			continue
		}
		break
	}
	return out, nil
}

// Flush 流结束时把缓冲中可发出的单元全部发出，尾部缺失按策略处理。
func (r *Reorderer) Flush() ([]Unit, error) {
	var out []Unit
	for len(r.buf) > 0 {
		p, ok := r.buf[r.head]
		if !ok {
			r.Stats.GapEvents++
			r.Stats.GapPackets++
			if r.Policy == PolicyStop {
				return out, fault.New(fault.KindTransport, fault.Fail, "rtp", 0, int(r.head),
					"tail gap at flush")
			}
			out = append(out, r.silenceUnit())
			r.head++
			continue
		}
		delete(r.buf, r.head)
		u, err := r.emit(p)
		if err != nil {
			return out, err
		}
		out = append(out, u)
		r.head++
	}
	return out, nil
}

func (r *Reorderer) emit(p *rtp.Packet) (Unit, error) {
	if r.hasTS && p.Timestamp < r.lastTS {
		return Unit{}, fault.New(fault.KindTransport, fault.Fail, "rtp", 0, int(p.SequenceNumber),
			"timestamp rollback: %d < %d", p.Timestamp, r.lastTS)
	}
	r.lastTS = p.Timestamp
	r.hasTS = true
	u := Unit{
		Packet:   p,
		Start:    clock.RTPTimestampToTicks(p.Timestamp, r.ClockRate),
		Duration: r.pktDur(),
	}
	r.lastEnd = u.Start + u.Duration
	return u, nil
}

func (r *Reorderer) silenceUnit() Unit {
	// 静音占据丢失包的媒体时间位置：时间线不压缩。
	u := Unit{Silence: true, Start: r.lastEnd, Duration: r.pktDur()}
	r.lastEnd = u.Start + u.Duration
	return u
}

// seqLess 判断 a 在 16 位回绕意义下是否落后于 b。
func seqLess(a, b uint16) bool {
	return a != b && int16(a-b) < 0
}
