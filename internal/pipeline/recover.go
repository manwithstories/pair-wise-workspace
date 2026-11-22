package pipeline

import (
	"hash/fnv"

	"mediad/internal/model"
)

// Recoverer 消费重排阶段产生的缺口事件，仅在存在校验一致的冗余样本时
// 才允许恢复；没有冗余或校验不一致一律标记为不可恢复的损坏区段，
// 严禁静默补零。
type Recoverer struct {
	// redundancy 按序号保存收到的冗余块。
	redundancy map[uint16]*Block
	recovered  int
	unrecovered int
	events     []model.Event
}

// NewRecoverer 构造恢复器。
func NewRecoverer() *Recoverer {
	return &Recoverer{redundancy: map[uint16]*Block{}}
}

// AddRedundant 登记一个冗余块（来自 FEC/重传通道）。
func (r *Recoverer) AddRedundant(b *Block) {
	r.redundancy[b.Seq] = b
}

// checksum 计算载荷的 FNV-1a 32 位摘要。
func checksum(p []byte) uint32 {
	h := fnv.New32a()
	h.Write(p)
	return h.Sum32()
}

// GapRequest 描述一个待裁决的缺块。
type GapRequest struct {
	SeqStart uint16
	SeqEnd   uint16
	// 缺口在统一采样刻度上的时间范围。
	TimeRange model.Range
	// 主通道在缺口两侧幸存块的校验参考（可为 0，表示无参考）。
	ReferenceChecksum uint32
}

// Resolve 对一段连续缺口逐块裁决，返回恢复结果事件。
// 只有当某序号存在冗余块且其校验自洽（携带的 Checksum 与重算值一致，
// 并且与参考一致或无参考）时才恢复；否则登记不可恢复事件。
func (r *Recoverer) Resolve(g GapRequest) []model.Event {
	var out []model.Event
	// 逐序号遍历（含 16 位回绕）。
	for seq := g.SeqStart; ; seq++ {
		ev := r.resolveOne(seq, g)
		out = append(out, ev)
		if ev.Severity == model.SevWarning {
			r.recovered++
		} else {
			r.unrecovered++
		}
		if seq == g.SeqEnd {
			break
		}
	}
	r.events = append(r.events, out...)
	return out
}

func (r *Recoverer) resolveOne(seq uint16, g GapRequest) model.Event {
	red, ok := r.redundancy[seq]
	if !ok {
		return model.NewEvent(model.CodeUnrecoverable, model.SevError, g.TimeRange,
			"no redundant sample available; zero-fill forbidden").
			WithSeq(int(seq), int(seq))
	}
	// 冗余块必须自洽：声明校验和与重算一致。
	if got := checksum(red.Payload); got != red.Checksum {
		return model.NewEvent(model.CodeUnrecoverable, model.SevError, g.TimeRange,
			"redundant payload checksum mismatch").
			WithSeq(int(seq), int(seq)).
			WithDetail("want", red.Checksum).WithDetail("got", got)
	}
	// 若有主通道参考，冗余块还须与参考一致。
	if g.ReferenceChecksum != 0 && red.Checksum != g.ReferenceChecksum {
		return model.NewEvent(model.CodeUnrecoverable, model.SevError, g.TimeRange,
			"redundant sample disagrees with surviving reference").
			WithSeq(int(seq), int(seq))
	}
	return model.NewEvent(model.CodeRecovered, model.SevWarning, g.TimeRange,
		"gap recovered from verified redundant sample").
		WithSeq(int(seq), int(seq))
}

// RecoveryRate 返回可恢复比例 recovered/(recovered+unrecovered)。
// 没有任何缺口时定义为 1.0。
func (r *Recoverer) RecoveryRate() float64 {
	total := r.recovered + r.unrecovered
	if total == 0 {
		return 1
	}
	return float64(r.recovered) / float64(total)
}

// Stats 返回已恢复与未恢复计数。
func (r *Recoverer) Stats() (recovered, unrecovered int) {
	return r.recovered, r.unrecovered
}

// Events 返回恢复器产生的全部事件。
func (r *Recoverer) Events() []model.Event { return r.events }

// CorruptRanges 把不可恢复事件按相邻时间范围合并为损坏区段摘要。
// 输入时钟用于把样本刻度换算为毫秒。
type MillisConverter func(samples uint64) uint64

// BuildCorruptRanges 聚合所有 SevError 事件的时间范围。
func BuildCorruptRanges(events []model.Event, toMs MillisConverter) []model.CorruptRange {
	var ranges []model.CorruptRange
	codeSet := map[int]map[model.Code]struct{}{}
	for _, ev := range events {
		if ev.Severity != model.SevError || ev.TimeRange.Empty() {
			continue
		}
		// 找到与当前事件相交或紧邻的既有区段并合并。
		merged := false
		for i := range ranges {
			cr := &ranges[i]
			rng := model.Range{Start: cr.StartSample, End: cr.EndSample}
			if rng.Overlap(ev.TimeRange) || rng.End == ev.TimeRange.Start || cr.StartSample == ev.TimeRange.End {
				cr.StartSample = min64(cr.StartSample, ev.TimeRange.Start)
				cr.EndSample = max64(cr.EndSample, ev.TimeRange.End)
				if codeSet[i] == nil {
					codeSet[i] = map[model.Code]struct{}{}
				}
				codeSet[i][ev.Code] = struct{}{}
				merged = true
				break
			}
		}
		if !merged {
			idx := len(ranges)
			ranges = append(ranges, model.CorruptRange{
				StartSample: ev.TimeRange.Start,
				EndSample:   ev.TimeRange.End,
			})
			codeSet[idx] = map[model.Code]struct{}{ev.Code: {}}
		}
	}
	out := make([]model.CorruptRange, 0, len(ranges))
	for i, cr := range ranges {
		cr.StartMs = toMs(cr.StartSample)
		cr.EndMs = toMs(cr.EndSample)
		for code := range codeSet[i] {
			cr.Codes = append(cr.Codes, code)
		}
		out = append(out, cr)
	}
	return out
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
