package ingest

import (
	"fmt"

	"media-pipeline/internal/fault"
	"media-pipeline/internal/media"
)

// EventKind 区分重排器输出事件的类型。
type EventKind uint8

const (
	// EventPacket 一个按序就绪的 RTP 包。
	EventPacket EventKind = iota
	// EventGap 等待期限到期后确认的突发缺块区间。
	EventGap
	// EventLate 缺口关闭后才到达的窗口外迟到包，仅记录不覆盖。
	EventLate
)

// Event 是重排器的单次输出。
type Event struct {
	Kind   EventKind
	Packet *ParsedPacket
	Gap    media.Gap
}

// ReorderConfig 配置有界乱序窗口与缺口等待期限。
type ReorderConfig struct {
	WindowSize uint16 // 有界乱序窗口（按 RTP 包序号计）
	MaxSeqGap  uint16 // 超过该跨度视为窗口溢出
	FramesPer  uint64 // 每个 RTP 包承载的固定采样帧数
	BlockID    string // 源块标识，用于错误溯源
}

// Reorderer 管理有界乱序窗口、去重与缺口期限。
type Reorderer struct {
	cfg ReorderConfig
	// received 以 64 位扩展序号为键保存乱序窗口内的包。
	received map[uint64]*ParsedPacket
	base     uint64 // 下一个期望输出的扩展序号
	hasBase  bool   // 首个包是否已到达（动态建立基准）
	sample0  uint64 // 基准包对应的全局采样帧序号
}

// NewReorderer 构造重排器。
func NewReorderer(cfg ReorderConfig) *Reorderer {
	return &Reorderer{cfg: cfg, received: make(map[uint64]*ParsedPacket)}
}

// extendSeq 将 16 位回绕序号扩展为以 base 为参照的单调 64 位序号。
func (r *Reorderer) extendSeq(seq16 uint16) uint64 {
	if !r.hasBase {
		return uint64(seq16)
	}
	base16 := uint16(r.base)
	d := uint16(seq16 - base16)
	if d <= 0x8000 {
		return r.base + uint64(d)
	}
	// 回绕到更早的周期
	return r.base - uint64(uint16(base16-seq16))
}

// seqToSample 将扩展序号换算为该包首帧的全局采样序号。
func (r *Reorderer) seqToSample(ext uint64) uint64 {
	return r.sample0 + ext*r.cfg.FramesPer
}

// Push 送入一个已解析的 RTP 包，返回新产生的有序事件。
// 重复包被去重；窗口外迟到包返回 EventLate 错误事件，绝不静默覆盖。
func (r *Reorderer) Push(p *ParsedPacket) ([]Event, error) {
	if p == nil {
		return nil, fault.New(fault.KindOther, r.cfg.BlockID, -1, -1, false,
			"nil RTP packet").WithOp("reorder.push")
	}
	if !r.hasBase {
		r.base = uint64(p.SequenceNumber)
		r.sample0 = 0
		r.hasBase = true
	}
	ext := r.extendSeq(p.SequenceNumber)
	switch {
	case ext < r.base:
		// 缺口可能已关闭：记为窗口外迟到，不覆盖任何状态。
		return []Event{{
			Kind:   EventLate,
			Packet: p,
			Gap: media.Gap{
				FirstSample: r.seqToSample(ext),
				Frames:      r.cfg.FramesPer,
				Reason:      "rtp-late-after-gap",
				FirstSeq:    p.SequenceNumber,
				LastSeq:     p.SequenceNumber,
			},
		}}, nil
	case ext > r.base+uint64(r.cfg.MaxSeqGap):
		return nil, fault.New(fault.KindGap, r.cfg.BlockID, -1,
			int64(r.seqToSample(ext)), true,
			fmt.Sprintf("RTP sequence %d overflows reorder window (base %d, max gap %d)",
				ext, r.base, r.cfg.MaxSeqGap)).WithOp("reorder.push")
	}
	if _, dup := r.received[ext]; dup || ext < r.base {
		// 已输出过的重复序号：去重，无事件。
		return nil, nil
	}
	r.received[ext] = p
	return r.drain(), nil
}

// drain 从基准序号开始连续输出就绪包；遇到空洞即停止，
// 空洞由 ExpireGaps 在等待期限到期后确认。
func (r *Reorderer) drain() []Event {
	var evs []Event
	for {
		p, ok := r.received[r.base]
		if !ok {
			return evs
		}
		delete(r.received, r.base)
		evs = append(evs, Event{Kind: EventPacket, Packet: p})
		r.base++
	}
}

// ExpireGaps 在等待期限到期（或收到超窗的后续包）时确认缺口。
// 它从当前基准扫描到已知最远序号，对连续空洞输出 EventGap，
// 随后推进基准，保证迟到包落入 EventLate 而非静默覆盖。
func (r *Reorderer) ExpireGaps() []Event {
	if !r.hasBase {
		return nil
	}
	var maxExt uint64
	for ext := range r.received {
		if ext > maxExt {
			maxExt = ext
		}
	}
	var evs []Event
	for r.base <= maxExt {
		if _, ok := r.received[r.base]; ok {
			break
		}
		// 合并连续空洞为一个缺口区间。
		first := r.base
		last := first
		for last+1 <= maxExt {
			if _, ok := r.received[last+1]; ok {
				break
			}
			last++
		}
		evs = append(evs, Event{
			Kind: EventGap,
			Gap: media.Gap{
				FirstSample: r.seqToSample(first),
				Frames:      (last - first + 1) * r.cfg.FramesPer,
				Reason:      "rtp-burst-loss",
				FirstSeq:    uint16(first),
				LastSeq:     uint16(last),
			},
		})
		r.base = last + 1
	}
	return append(evs, r.drain()...)
}

// Flush 在流结束时确认所有残留缺口并输出窗口内剩余包。
func (r *Reorderer) Flush() []Event { return r.ExpireGaps() }

// Pending 返回窗口内滞留（乱序未输出）的包数。
func (r *Reorderer) Pending() int { return len(r.received) }

// SetSampleOrigin 由上层在建立基准后设定全局采样原点。
func (r *Reorderer) SetSampleOrigin(sample0 uint64) { r.sample0 = sample0 }
