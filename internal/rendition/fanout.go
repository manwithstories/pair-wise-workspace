// Package rendition 把统一时间线扇出为多个独立表示层，每层独立核验时钟
// 单调性并在丢包缺口处切分可独立恢复的段；扇入阶段再校验半开区间边界与
// 跨层偏差。各层只读取时间线的不可变样本，彼此无共享可变状态，可安全并发。
package rendition

import (
	"runtime"
	"sort"
	"sync"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/timeline"
)

// QueueCap 是扇出作业与结果通道的硬上限，满足并发队列受限约束。
const QueueCap = 64

// Tier 描述一档表示层的抽取节奏。
type Tier struct {
	ID     uint8
	Name   string
	Stride uint64 // 样本序号按 stride 取模抽帧，1 表示全量
}

// DefaultTiers 是三档标准表示层：全量 / 半速 / 四分之一速。
func DefaultTiers() []Tier {
	return []Tier{
		{ID: 0, Name: "full", Stride: 1},
		{ID: 1, Name: "half", Stride: 2},
		{ID: 2, Name: "quarter", Stride: 4},
	}
}

// Segment 是表示层内一条轨道上的连续可恢复段，时间区间为半开 [StartUS,EndUS)。
type Segment struct {
	RenditionID uint8
	TrackID     uint8
	Index       int
	StartUS     int64
	EndUS       int64
	FirstSeq    uint64
	LastSeq     uint64
	Samples     []model.Sample
}

// Rendition 是单档表示层的独立恢复产物。
type Rendition struct {
	Tier     Tier
	Segments []Segment
	// 每轨覆盖跨度，供扇入做跨层一致性核验。
	Span map[uint8][2]int64 // trackID -> [minStartUS, maxEndUS)
	// 每轨标称帧周期（微秒），供扇入按层自适应容差。
	PeriodUS map[uint8]int64
}

// buildRendition 串行构建单档表示层：独立核验时钟单调性，按缺口切分恢复段。
func buildRendition(tier Tier, tl *timeline.Timeline, fc *fault.Collector) (*Rendition, error) {
	r := &Rendition{Tier: tier, Span: map[uint8][2]int64{}, PeriodUS: map[uint8]int64{}}
	for _, tp := range tl.Plan.Tracks {
		r.PeriodUS[tp.ID] = tp.PeriodUS
		samples := tl.ByTrack[tp.ID]
		gaps := gapsOf(tl.Gaps, tp.ID)
		segs := segmentize(tier, tp.ID, samples, gaps, fc)
		r.Segments = append(r.Segments, segs...)
		if len(segs) > 0 {
			r.Span[tp.ID] = [2]int64{segs[0].StartUS, segs[len(segs)-1].EndUS}
		}
	}
	// 段顺序确定性：轨道优先、段序号次之。
	sort.Slice(r.Segments, func(i, j int) bool {
		if r.Segments[i].TrackID != r.Segments[j].TrackID {
			return r.Segments[i].TrackID < r.Segments[j].TrackID
		}
		return r.Segments[i].Index < r.Segments[j].Index
	})
	return r, nil
}

// gapsOf 取出指定轨道的缺口，保持时间线已排好的顺序。
func gapsOf(all []model.Gap, track uint8) []model.Gap {
	out := make([]model.Gap, 0)
	for _, g := range all {
		if g.TrackID == track {
			out = append(out, g)
		}
	}
	return out
}

// segmentize 抽取本层样本并在缺口处切段，同时独立核验半开区间不重叠。
func segmentize(tier Tier, track uint8, samples []model.Sample,
	gaps []model.Gap, fc *fault.Collector) []Segment {
	var segs []Segment
	var cur *Segment
	var lastSeq uint64
	flush := func() {
		if cur != nil && len(cur.Samples) > 0 {
			last := cur.Samples[len(cur.Samples)-1]
			cur.EndUS = last.PTS + last.Duration
			cur.LastSeq = last.Seq
			// 整数换算下单个样本时长可能为 0，零长段不构成可恢复单元，丢弃。
			if cur.EndUS > cur.StartUS {
				segs = append(segs, *cur)
			}
		}
		cur = nil
	}
	for _, s := range samples {
		if s.Seq%tier.Stride != 0 {
			continue
		}
		// 独立时钟核验：归一化 PTS 必须单调不减。
		if cur != nil {
			prev := cur.Samples[len(cur.Samples)-1]
			if s.PTS < prev.PTS {
				fc.Record(fault.KindBoundary, track, s.Seq, 0, 0,
					"rendition clock moved backwards")
				flush()
			} else if s.PTS < prev.PTS+prev.Duration {
				// 半开区间重叠是致命级边界错误。
				fc.Record(fault.KindBoundary, track, s.Seq, 0, 0,
					"half-open intervals overlap")
			}
		}
		// 跨过任一缺口即闭合当前段，缺口之后独立恢复为新段。
		if cur != nil && crossesGap(gaps, lastSeq, s.Seq) {
			flush()
		}
		if cur == nil {
			cur = &Segment{
				RenditionID: tier.ID, TrackID: track, Index: len(segs),
				StartUS: s.PTS, FirstSeq: s.Seq,
			}
		}
		cur.Samples = append(cur.Samples, s)
		lastSeq = s.Seq
	}
	flush()
	return segs
}

// crossesGap 判定相邻两个抽帧样本之间是否夹着至少一个丢包缺口。
func crossesGap(gaps []model.Gap, prevSeq, nextSeq uint64) bool {
	for _, g := range gaps {
		if prevSeq <= g.AfterSeq && nextSeq >= g.BeforeSeq {
			return true
		}
	}
	return false
}

// FanOut 并发构建全部表示层。并发度与作业队列都有硬上限，
// 产物按 Tier.ID 排序，与同输入的串行构建逐字节一致。
func FanOut(tl *timeline.Timeline, tiers []Tier, fc *fault.Collector) ([]*Rendition, error) {
	jobs := make(chan Tier, QueueCap)
	results := make(chan *Rendition, len(tiers))
	errCh := make(chan error, 1)
	workers := runtime.NumCPU()
	if workers > len(tiers) {
		workers = len(tiers)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tier := range jobs {
				r, err := buildRendition(tier, tl, fc)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				results <- r
			}
		}()
	}
	for _, t := range tiers {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	close(results)
	select {
	case err := <-errCh:
		return nil, err
	default:
	}
	out := make([]*Rendition, 0, len(tiers))
	for r := range results {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tier.ID < out[j].Tier.ID })
	return out, nil
}

// FanOutSerial 是 FanOut 的确定性串行参照，专供测试与可复现核验使用。
func FanOutSerial(tl *timeline.Timeline, tiers []Tier, fc *fault.Collector) ([]*Rendition, error) {
	out := make([]*Rendition, 0, len(tiers))
	for _, t := range tiers {
		r, err := buildRendition(t, tl, fc)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
