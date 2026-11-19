package rtp

import (
	"sort"

	"github.com/pion/rtp"

	"pairflow/internal/clock"
)

// PayloadType 是本流水线约定支持的 PCM 负载映射（PT=96 → 线性 PCM）。
const PayloadType = uint8(96)

// Packet 是重排后的单包视图，保留源字节偏移与到达时间。
type Packet struct {
	DeviceID    string
	Seq         uint64 // 扩展后的 64bit 序号
	RawSeq      uint16
	Timestamp   uint32 // 原始 32bit RTP 时间戳
	Payload     []byte
	ArrivalNano int64
	FileOffset  int64
}

// Conflict 记录“同序号不同内容”的重复包冲突及两处源偏移。
type Conflict struct {
	DeviceID     string
	Seq          uint64
	FirstOffset  int64
	SecondOffset int64
}

// Gap 描述突发丢失形成的序号区间，并给出基于时间戳的缺失采样估算。
type Gap struct {
	DeviceID       string
	StartSeq       uint64
	EndSeq         uint64
	MissingPackets int64
	EstSamples     int64
	LastGoodOffset int64
	NextGoodOffset int64
}

// Stats 汇总单设备重排阶段的质量指标。
type Stats struct {
	DeviceID         string
	Total            int64
	Duplicates       int64
	Conflicts        int64
	Reordered        int64
	Gaps             []Gap
	CacheCheckpoints int64
	BandwidthBps     float64
	WaitPackets      int
}

// Config 控制重排行为。零值会在 NewReorderer 时替换为默认值。
type Config struct {
	MaxCacheBytes   int64 // 单轨缓存上限（规格默认 256MB）
	BaseWaitPackets int   // 基础有界等待（包数）
	MinWaitPackets  int
	MaxWaitPackets  int
	// OnCheckpoint 在缓存超限需要落盘检查点时回调；返回错误则终止该轨。
	OnCheckpoint func(deviceID string, cachedBytes int64) error
}

// DefaultConfig 返回规格默认配置。
func DefaultConfig() Config {
	return Config{
		MaxCacheBytes:   256 * 1024 * 1024,
		BaseWaitPackets: 32,
		MinWaitPackets:  4,
		MaxWaitPackets:  128,
	}
}

type entry struct {
	pkt    Packet
	arrive int64
	size   int
}

// Reorderer 对单设备流做有界重排、去重与缺口估算。
type Reorderer struct {
	cfg     Config
	ext     clock.SeqExtender
	bw      *bwEstimator
	buf     map[uint64]entry
	arrival int64
	maxSeq  uint64 // 已观测到的最大扩展序号
	haveSeen bool
	lowest  uint64 // 期望输出的下一个序号
	haveBase bool
	cached  int64
	stats   Stats
	deltas  []int64 // 连续输出包的 RTP 时间戳（用于中位数增量）
	median  int64
}

// NewReorderer 创建单设备重排器。
func NewReorderer(deviceID string, cfg Config) *Reorderer {
	if cfg.MaxCacheBytes <= 0 {
		cfg.MaxCacheBytes = 256 * 1024 * 1024
	}
	if cfg.BaseWaitPackets <= 0 {
		cfg.BaseWaitPackets = 32
	}
	if cfg.MinWaitPackets <= 0 {
		cfg.MinWaitPackets = 4
	}
	if cfg.MaxWaitPackets <= 0 {
		cfg.MaxWaitPackets = 128
	}
	return &Reorderer{
		cfg: cfg, bw: newBWEstimator(), buf: make(map[uint64]entry),
		stats: Stats{DeviceID: deviceID, WaitPackets: cfg.BaseWaitPackets},
	}
}

// waitBudget 依据滑动窗口带宽估算动态调整有界等待（包数）。
func (r *Reorderer) waitBudget() int {
	w := r.cfg.BaseWaitPackets
	if bps := r.bw.bandwidth(); bps > 0 {
		scale := r.bw.referenceBps() / bps
		if scale < 0.25 {
			scale = 0.25
		}
		if scale > 4 {
			scale = 4
		}
		w = int(float64(r.cfg.BaseWaitPackets) * scale)
	}
	if w < r.cfg.MinWaitPackets {
		w = r.cfg.MinWaitPackets
	}
	if w > r.cfg.MaxWaitPackets {
		w = r.cfg.MaxWaitPackets
	}
	r.stats.WaitPackets = w
	return w
}

// Push 摄入一条采集记录对应的原始 RTP 报文，返回本次可按序输出的包。
// 重复包按内容校验；冲突副本同时保留两处源偏移。
func (r *Reorderer) Push(rec CaptureRecord) ([]Packet, []Conflict, error) {
	r.stats.Total++
	r.arrival++
	r.bw.observe(rec.ArrivalNano, len(rec.Packet))
	r.stats.BandwidthBps = r.bw.bandwidth()

	raw := &rtp.Packet{}
	if err := raw.Unmarshal(rec.Packet); err != nil {
		return nil, nil, clock.Wrap("rtp.parse", clock.KindUnsupportedPayload,
			rec.DeviceID, rec.FileOffset, clock.NoSeq, "invalid rtp header",
			"跳过该包", err)
	}
	if raw.PayloadType != PayloadType {
		return nil, nil, clock.New("rtp.parse", clock.KindUnsupportedPayload,
			rec.DeviceID, rec.FileOffset, clock.NoSeq,
			"unsupported payload type (only PT=96 PCM mapped)", "跳过该包")
	}
	ext, _ := r.ext.Update(raw.SequenceNumber)
	pkt := Packet{
		DeviceID: rec.DeviceID, Seq: ext, RawSeq: raw.SequenceNumber,
		Timestamp: raw.Timestamp, Payload: raw.Payload,
		ArrivalNano: rec.ArrivalNano, FileOffset: rec.FileOffset,
	}
	var conflicts []Conflict
	// 已在缓存中：重复包，按内容校验。
	if old, ok := r.buf[ext]; ok {
		r.stats.Duplicates++
		if !bytesEqual(old.pkt.Payload, pkt.Payload) {
			r.stats.Conflicts++
			conflicts = append(conflicts, Conflict{
				DeviceID: rec.DeviceID, Seq: ext,
				FirstOffset: old.pkt.FileOffset, SecondOffset: rec.FileOffset})
		}
		return nil, conflicts, nil
	}
	// 已输出序号上的迟到重传：同样按重复处理。
	if r.haveBase && ext < r.lowest {
		r.stats.Duplicates++
		return nil, conflicts, nil
	}
	if r.haveSeen && ext < r.maxSeq {
		// 序号小于历史最大值且非重复：迟到的乱序包。
		r.stats.Reordered++
	}
	if !r.haveSeen || ext > r.maxSeq {
		r.maxSeq = ext
		r.haveSeen = true
	}
	r.buf[ext] = entry{pkt: pkt, arrive: r.arrival, size: len(rec.Packet)}
	r.cached += int64(len(rec.Packet))

	if err := r.enforceCacheLimit(); err != nil {
		return nil, conflicts, err
	}
	return r.advance(false), conflicts, nil
}

// enforceCacheLimit 在缓存超限时先尝试落盘检查点；仍超限则强制推进等待窗口。
func (r *Reorderer) enforceCacheLimit() error {
	if r.cached <= r.cfg.MaxCacheBytes || len(r.buf) == 0 {
		return nil
	}
	if r.cfg.OnCheckpoint != nil {
		if err := r.cfg.OnCheckpoint(r.stats.DeviceID, r.cached); err != nil {
			return err
		}
		r.stats.CacheCheckpoints++
	}
	for r.cached > r.cfg.MaxCacheBytes && len(r.buf) > 0 {
		keys := r.sortedKeys()
		oldest := keys[0]
		if !r.haveBase {
			r.lowest = oldest
			r.haveBase = true
		}
		// 把最旧序号推进一格，必要时声明缺口，直到缓存回落。
		r.releaseThrough(oldest)
	}
	return nil
}

// Flush 在输入结束时输出全部剩余缓存，尾部缺口同样显式声明。
func (r *Reorderer) Flush() ([]Packet, []Gap) {
	out := r.advance(true)
	return out, r.stats.Gaps
}

// advance 按有界等待窗口推进输出；flush=true 时清空全部缓存。
func (r *Reorderer) advance(flush bool) []Packet {
	if len(r.buf) == 0 {
		return nil
	}
	keys := r.sortedKeys()
	if !r.haveBase {
		r.lowest = keys[0]
		r.haveBase = true
	}
	var horizon uint64
	if flush {
		horizon = keys[len(keys)-1]
	} else {
		horizon = r.maxSeq - uint64(r.waitBudget())
	}
	var out []Packet
	for _, k := range keys {
		if k > horizon {
			break
		}
		if k > r.lowest {
			r.declareGap(r.lowest, k)
		}
		e := r.buf[k]
		r.rememberTS(e.pkt.Timestamp)
		out = append(out, e.pkt)
		delete(r.buf, k)
		r.cached -= int64(e.size)
		r.lowest = k + 1
	}
	return out
}

// releaseThrough 强制把序号 <=seq 的缓存处理掉（用于缓存超限），缺口照常声明。
func (r *Reorderer) releaseThrough(seq uint64) {
	for {
		keys := r.sortedKeys()
		if len(keys) == 0 || keys[0] > seq {
			return
		}
		k := keys[0]
		if k > r.lowest {
			r.declareGap(r.lowest, k)
		}
		e := r.buf[k]
		delete(r.buf, k)
		r.cached -= int64(e.size)
		r.lowest = k + 1
	}
}

func (r *Reorderer) declareGap(from, to uint64) {
	if to <= from {
		return
	}
	g := Gap{DeviceID: r.stats.DeviceID, StartSeq: from, EndSeq: to,
		MissingPackets: int64(to - from)}
	d := r.medianDelta()
	g.EstSamples = g.MissingPackets * d
	if g.EstSamples < g.MissingPackets {
		g.EstSamples = g.MissingPackets
	}
	r.stats.Gaps = append(r.stats.Gaps, g)
}

// rememberTS 记录输出流中相邻包的 RTP 时间戳，供缺口采样估算使用。
func (r *Reorderer) rememberTS(ts uint32) { r.deltas = append(r.deltas, int64(int32(ts))) }

func (r *Reorderer) medianDelta() int64 {
	if r.median > 0 {
		return r.median
	}
	if len(r.deltas) < 2 {
		return 1
	}
	diff := make([]int64, 0, len(r.deltas)-1)
	for i := 1; i < len(r.deltas); i++ {
		if d := r.deltas[i] - r.deltas[i-1]; d > 0 {
			diff = append(diff, d)
		}
	}
	if len(diff) == 0 {
		return 1
	}
	sort.Slice(diff, func(i, j int) bool { return diff[i] < diff[j] })
	r.median = diff[len(diff)/2]
	return r.median
}

func (r *Reorderer) sortedKeys() []uint64 {
	keys := make([]uint64, 0, len(r.buf))
	for k := range r.buf {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// Stats 返回当前累计统计（含动态带宽与等待预算快照）。
func (r *Reorderer) Stats() Stats {
	r.stats.BandwidthBps = r.bw.bandwidth()
	r.stats.WaitPackets = r.waitBudget()
	return r.stats
}

// RestoreStats 在检查点续跑时恢复统计基线（缺口列表同样带回）。
func (r *Reorderer) RestoreStats(s Stats) {
	r.stats = s
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
