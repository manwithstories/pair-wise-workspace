// Package analyzer 端到端解析 MPEG-TS 字节流：包识别 → PSI 校验/切换 →
// PES 重组 → 适配器解码 → 统一时间线，并汇总传输损坏指标。
package analyzer

import (
	"fmt"

	"tsproof/internal/adapter"
	"tsproof/internal/clock"
	"tsproof/internal/codec"
	"tsproof/internal/pes"
	"tsproof/internal/ts"
)

// Metrics 汇总一次解析观察到的传输与解析事件计数。
type Metrics struct {
	Packets       int
	PayloadPkts   int
	CCGaps        int // 连续性缺口次数
	Duplicates    int // 重复包次数
	Reorders      int // 乱序次数
	CRCReject     int // PSI CRC 拒绝次数
	PSISwitches   int // 合法 PSI 版本切换次数
	TruncatedPES  int // 截断 PES 数
	PCRWraps      int // PCR 回绕展开次数
	Untrusted     int // 不可信块数
	UnknownTypes  int // 未知 stream_type 次数
	AdapterErrors int // 适配器解码失败次数
}

// StreamMap 是当前生效的 PSI 轨道映射（仅在完整且 CRC 正确的版本上切换）。
type StreamMap struct {
	Version       uint8
	PMTPID        uint16
	PCRPID        uint16
	StreamTypePID map[uint8]uint16 // stream_type -> PID
	PIDType       map[uint16]uint8 // PID -> stream_type
}

// Result 是一次完整解析的产出。
type Result struct {
	Metrics Metrics
	Map     *StreamMap
	Timeline *clock.Timeline
	// 每个 PID 解出的块（图像/音频），按轨道索引顺序。
	Blocks   map[uint16][]*adapter.Block
	Errors   []error // 可恢复错误（CRC、缺口等），不中断解析
}

// Analyzer 持有解析过程状态。
type Analyzer struct {
	registry *adapter.Registry
	tracker  *ts.CCTracker
	sections map[uint16]*ts.SectionAssembler
	pesAsm   *pes.Assembler
	timeline *clock.Timeline
	pcrUnwrap map[uint16]pcrState
	ptsUnwrap map[uint16]ptsState
	blockIdx map[uint16]int
	blocks   map[uint16][]*adapter.Block
	current  *StreamMap
	metrics  Metrics
	errs     []error
}

type pcrState struct {
	prev    clock.Tick
	lastRaw clock.Tick
	ok      bool
}

type ptsState struct {
	prev clock.Tick
	ok   bool
}

// New 创建带默认 JPEG/PCM 适配器的分析器。
func New() *Analyzer {
	reg := adapter.NewRegistry()
	reg.MustRegister(codec.NewJPEG())
	reg.MustRegister(codec.NewPCM())
	return &Analyzer{
		registry: reg,
		tracker:  ts.NewCCTracker(),
		sections: map[uint16]*ts.SectionAssembler{
			ts.PIDPAT: ts.NewSectionAssembler(),
		},
		pesAsm:    pes.NewAssembler(),
		timeline:  clock.NewTimeline(),
		pcrUnwrap: map[uint16]pcrState{},
		ptsUnwrap: map[uint16]ptsState{},
		blockIdx:  map[uint16]int{},
		blocks:    map[uint16][]*adapter.Block{},
	}
}

// Parse 解析完整 TS 字节流并返回结果。流被截断时不报错，
// 截断的 PES 通过 Flush 计入 TruncatedPES。
func (a *Analyzer) Parse(buf []byte) (*Result, error) {
	pkts, _, err := ts.SplitPackets(buf)
	if err != nil {
		return nil, fmt.Errorf("analyzer: split: %w", err)
	}
	for order, raw := range pkts {
		p, perr := ts.ParsePacket(raw, order)
		if perr != nil {
			return nil, fmt.Errorf("analyzer: packet %d: %w", order, perr)
		}
		a.handlePacket(p)
	}
	a.flushPES()
	return &Result{
		Metrics:  a.metrics,
		Map:      a.current,
		Timeline: a.timeline,
		Blocks:   a.blocks,
		Errors:   a.errs,
	}, nil
}

func (a *Analyzer) handlePacket(p *ts.Packet) {
	a.metrics.Packets++
	if p.IsNull() {
		return
	}
	// PCR 回绕展开（任何携带 PCR 的包）。
	if p.HasPCR() {
		a.unwrapPCR(p.PID, clock.Tick(p.PCR))
	}
	// PSI 通道：PAT 与已知 PMT PID。
	if p.PID == ts.PIDPAT || (a.current != nil && p.PID == a.current.PMTPID) {
		a.handlePSI(p)
		return
	}
	if !p.HasPayload {
		return
	}
	a.metrics.PayloadPkts++
	// 连续性检查。
	ev := a.tracker.Observe(p)
	if ev.Duplicate {
		a.metrics.Duplicates++
	}
	if ev.Gap > 0 {
		a.metrics.CCGaps += ev.Gap
		a.markTrackUntrusted(p.PID)
	}
	if ev.Reversed {
		a.metrics.Reorders++
	}
	if a.current == nil {
		return // 尚无有效 PMT，无法映射媒体 PID
	}
	if _, known := a.current.PIDType[p.PID]; !known {
		return
	}
	a.handlePES(p, ev)
}
