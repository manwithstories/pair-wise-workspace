package pipeline

import (
	"sync"

	"github.com/pion/rtp"

	"mediad/internal/model"
	rtprx "mediad/internal/rtp"
)

// Stage 表示管线中的处理阶段名，用于事件来源标注。
type Stage string

const (
	StageRTP      Stage = "rtp"
	StageWAV      Stage = "wav"
	StageInspect  Stage = "mp4"
	StageRecover  Stage = "recover"
)

// Config 固定管线的有界资源，保证内存峰值可推导。
type Config struct {
	QueueCapacity int   // 有界队列容量（块数）
	MaxBlockBytes int64 // 单块解析缓冲上限（1 MiB）
	ReorderWindow int   // RTP 重排窗口（<=4096）
}

// Pipeline 串联 RTP 接收→重排去重→对齐→检查→指标。
// 所有共享状态由同一把锁保护，事件通过统一错误模型传播。
type Pipeline struct {
	cfg      Config
	queue    *BoundedQueue
	reorder  *rtprx.ReorderBuffer
	recoverer *Recoverer
	bw       *BandwidthEstimator

	mu     sync.Mutex
	events []model.Event
	closed bool
}

// New 构造管线；非法配置返回错误。
func New(cfg Config) (*Pipeline, error) {
	if cfg.MaxBlockBytes <= 0 {
		cfg.MaxBlockBytes = 1 << 20
	}
	rb, err := rtprx.NewReorderBuffer(cfg.ReorderWindow)
	if err != nil {
		return nil, err
	}
	q, err := NewBoundedQueue(cfg.QueueCapacity)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		cfg:       cfg,
		queue:     q,
		reorder:   rb,
		recoverer: NewRecoverer(),
		bw:        NewBandwidthEstimator(0, cfg.ReorderWindow, cfg.ReorderWindow/2+1),
	}, nil
}

// IngestRTP 接收一个 RTP 包：重排、去重，冲突/迟到转为统一事件。
// sampleOf 把包的 RTP 时间戳映射到统一采样刻度，使事件携带非空时间范围；
// 传 nil 则事件不绑定时间区间。成功入窗的包由 DrainOrdered 按序产出。
func (p *Pipeline) IngestRTP(pkt *rtp.Packet, sampleOf func(ts uint32) uint64, blockSize uint64) error {
	err := p.reorder.Push(pkt)
	// 事件时间范围：以包时间戳为起点、单包跨度为终点的闭开区间。
	rng := model.Range{}
	if sampleOf != nil {
		lo := sampleOf(pkt.Timestamp)
		rng = model.Range{Start: lo, End: lo + blockSize}
	}
	switch err {
	case nil:
		return nil
	case rtprx.ErrDuplicate:
		p.addEvent(model.NewEvent(model.CodeDupConflict, model.SevInfo,
			rng, "identical duplicate packet ignored").
			WithSeq(int(pkt.SequenceNumber), int(pkt.SequenceNumber)))
		return nil
	case rtprx.ErrConflict:
		p.addEvent(model.NewEvent(model.CodeDupConflict, model.SevError,
			rng, "same sequence number with conflicting payload").
			WithSeq(int(pkt.SequenceNumber), int(pkt.SequenceNumber)))
		return err
	case rtprx.ErrLateOutside:
		p.addEvent(model.NewEvent(model.CodeLateOutside, model.SevError,
			rng, "late packet outside reorder window; unrecoverable").
			WithSeq(int(pkt.SequenceNumber), int(pkt.SequenceNumber)))
		return err
	default:
		return err
	}
}

// DrainOrdered 把当前可按序交付的包转成 Block 入有界队列。
// sampleOf 把 RTP 时间戳映射到统一采样刻度（由调用方的 clock 提供）。
func (p *Pipeline) DrainOrdered(sampleOf func(ts uint32) uint64, blockSize int) int {
	pkts := p.reorder.PopAll()
	for _, pkt := range pkts {
		lo := sampleOf(pkt.Timestamp)
		hi := lo + uint64(blockSize)
		p.queue.Push(&Block{
			Seq:      pkt.SequenceNumber,
			TS:       pkt.Timestamp,
			SampleLo: lo,
			SampleHi: hi,
			Payload:  append([]byte(nil), pkt.Payload...),
			Checksum: checksum(pkt.Payload),
		})
	}
	return len(pkts)
}

// RecordGaps 把重排缓冲识别到的缺口交给恢复器裁决。
// sampleOf 用于把缺口两侧 RTP 时间戳映射为时间范围。
func (p *Pipeline) RecordGaps(sampleOf func(ts uint32) uint64) {
	for _, g := range p.reorder.Gaps() {
		lo := sampleOf(g.PrevTS)
		hi := lo
		if g.HasNext {
			hi = sampleOf(g.NextTS)
		}
		req := GapRequest{
			SeqStart:  g.SeqStart,
			SeqEnd:    g.SeqEnd,
			TimeRange: model.Range{Start: lo, End: max64(hi, lo+1)},
		}
		evs := p.recoverer.Resolve(req)
		p.addEvents(evs)
		// 突发丢失本身也登记一条来源事件。
		p.addEvent(model.NewEvent(model.CodeBurstLoss, model.SevWarning,
			req.TimeRange, "burst loss detected").
			WithSeq(int(g.SeqStart), int(g.SeqEnd)))
	}
}

// AddRedundant 透传冗余块给恢复器。
func (p *Pipeline) AddRedundant(b *Block) { p.recoverer.AddRedundant(b) }

// NoteAVDiscontinuity 登记音频缺口跨越视频关键帧的同源事件。
func (p *Pipeline) NoteAVDiscontinuity(r model.Range, msg string) {
	p.addEvent(model.NewEvent(model.CodeAVDiscontinuity, model.SevError, r, msg))
}

// NoteTimestampRollback 登记 RTP 时间戳回退事件。
func (p *Pipeline) NoteTimestampRollback(r model.Range, seq uint16) {
	p.addEvent(model.NewEvent(model.CodeTimestampRoll, model.SevError, r,
		"RTP timestamp rolled back").WithSeq(int(seq), int(seq)))
}

// NoteExternal 登记来自其它阶段（如 WAV 解析）的统一事件。
func (p *Pipeline) NoteExternal(e model.Event) { p.addEvent(e) }

// Queue 暴露有界队列供消费者读取。
func (p *Pipeline) Queue() *BoundedQueue { return p.queue }

// Config 返回管线的固定有界资源配置。
func (p *Pipeline) Config() Config { return p.cfg }

// Recoverer 暴露恢复器以读取恢复率。
func (p *Pipeline) Recoverer() *Recoverer { return p.recoverer }

// Reorder 暴露重排缓冲以读取峰值与统计。
func (p *Pipeline) Reorder() *rtprx.ReorderBuffer { return p.reorder }

// Bandwidth 暴露带宽估算器。
func (p *Pipeline) Bandwidth() *BandwidthEstimator { return p.bw }

// Events 返回累积的统一事件快照。
func (p *Pipeline) Events() []model.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]model.Event, len(p.events))
	copy(out, p.events)
	return out
}

func (p *Pipeline) addEvent(e model.Event) {
	p.mu.Lock()
	p.events = append(p.events, e)
	p.mu.Unlock()
}

func (p *Pipeline) addEvents(es []model.Event) {
	p.mu.Lock()
	p.events = append(p.events, es...)
	p.mu.Unlock()
}

// CorruptRanges 基于当前事件构建损坏区段。
func (p *Pipeline) CorruptRanges(toMs MillisConverter) []model.CorruptRange {
	return BuildCorruptRanges(p.Events(), toMs)
}
