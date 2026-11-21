package clock

import "sort"

// TimelineConfig 控制共享媒体时间基准的构建参数。
type TimelineConfig struct {
	AudioRate            int64 // 音频采样率，如 48000
	IsolationThresholdNS int64 // 锚点残差超过该值则切分时钟段
	MinAnchorsPerSegment int   // 段内最少锚点数，不足则标记隔离
}

// ClockSegment 是一段连续的时钟拟合区间。
type ClockSegment struct {
	StartSeq uint32
	EndSeq   uint32
	Isolated bool
	Fit      SegmentFit
	rec      *Reconstructor
}

// Timeline 是所有表示层共享的媒体时间基准。
// 它以音频累计采样数为锚点分段拟合 RTP 时钟，无法拟合的段标记为隔离段，
// 隔离段内的帧只给标称外推时间并显式暴露，禁止用于分片边界发布。
type Timeline struct {
	cfg      TimelineConfig
	points   []timelinePoint
	segs     []ClockSegment
	maxResNS int64
}

type timelinePoint struct {
	seq     uint32
	tick    int64
	audioNS int64
}

// NewTimeline 构造共享时间线并填入安全默认值。
func NewTimeline(cfg TimelineConfig) *Timeline {
	if cfg.AudioRate <= 0 {
		cfg.AudioRate = 48000
	}
	if cfg.IsolationThresholdNS <= 0 {
		cfg.IsolationThresholdNS = 20_000_000
	}
	if cfg.MinAnchorsPerSegment < 2 {
		cfg.MinAnchorsPerSegment = 2
	}
	return &Timeline{cfg: cfg}
}

// AddPoint 录入一帧的扩展 tick 与音频锚点，返回锚点时间是否相对前点倒退。
func (t *Timeline) AddPoint(seq uint32, extTick int64, audioSamples uint32) bool {
	audioNS := int64(audioSamples) * 1_000_000_000 / t.cfg.AudioRate
	back := len(t.points) > 0 && audioNS < t.points[len(t.points)-1].audioNS
	t.points = append(t.points, timelinePoint{seq: seq, tick: extTick, audioNS: audioNS})
	return back
}

// Build 顺序扫描锚点并做前缀拟合：当前段拟合直线对下一锚点的预测
// 残差超过阈值即判定时钟突变。突变点及其后 MinAnchors 帧构成守卫带，
// 标记为隔离段——这些帧横跨时间不连续点，禁止用于发布边界；
// 守卫带之后重新积累锚点拟合新段。末尾不足最小学数的残段同样隔离。
func (t *Timeline) Build() error {
	sort.Slice(t.points, func(i, j int) bool { return t.points[i].seq < t.points[j].seq })
	minA := t.cfg.MinAnchorsPerSegment
	var cur []Anchor
	curStart := 0
	flushFit := func() {
		if len(cur) == 0 {
			return
		}
		seg := ClockSegment{
			StartSeq: t.points[curStart].seq,
			EndSeq:   t.points[curStart+len(cur)-1].seq,
		}
		if len(cur) < minA {
			seg.Isolated = true
		} else if fit, err := FitRate(cur); err != nil {
			seg.Isolated = true
		} else {
			base := cur[0]
			seg.Fit = fit
			seg.rec = NewReconstructor(fit, base.Tick, base.NS)
			if fit.MaxResNS > t.maxResNS {
				t.maxResNS = fit.MaxResNS
			}
		}
		t.segs = append(t.segs, seg)
	}
	for i, p := range t.points {
		if len(cur) >= minA {
			fit, err := FitRate(cur)
			if err == nil {
				pred := fit.Intercept + fit.Slope*float64(p.tick)
				res := pred - float64(p.audioNS)
				if res < 0 {
					res = -res
				}
				if int64(res) > t.cfg.IsolationThresholdNS {
					flushFit()
					cur = nil
					// 守卫带：突变点起 minA 帧隔离，之后重新拟合。
					bandEnd := i + minA
					if bandEnd > len(t.points) {
						bandEnd = len(t.points)
					}
					t.segs = append(t.segs, ClockSegment{
						StartSeq: t.points[i].seq,
						EndSeq:   t.points[bandEnd-1].seq,
						Isolated: true,
					})
					curStart = bandEnd
					i = bandEnd - 1 // 主循环 i++ 后从守卫带之后继续
					continue
				}
			}
		}
		if len(cur) == 0 {
			curStart = i
		}
		cur = append(cur, Anchor{Tick: p.tick, NS: p.audioNS})
	}
	flushFit()
	return nil
}

// MaxResidual 返回全部可拟合段中的最大锚点残差（纳秒）。
func (t *Timeline) MaxResidual() int64 { return t.maxResNS }

// Segments 暴露时钟段（含隔离标记），供边界核验使用。
func (t *Timeline) Segments() []ClockSegment { return t.segs }

// IsolatedAt 报告指定帧是否落在隔离时钟段。
func (t *Timeline) IsolatedAt(seq uint32) bool {
	for i := range t.segs {
		s := &t.segs[i]
		if seq >= s.StartSeq && seq <= s.EndSeq {
			return s.Isolated
		}
	}
	return true
}

// MediaNS 返回帧的统一媒体时间；第二个返回值为 true 表示该帧落在隔离段，
// 此时仅给出 90kHz 标称外推时间，禁止作为发布边界依据。
func (t *Timeline) MediaNS(seq uint32, extTick int64) (int64, bool) {
	for i := range t.segs {
		s := &t.segs[i]
		if seq >= s.StartSeq && seq <= s.EndSeq {
			if s.Isolated {
				return t.nominalNS(extTick), true
			}
			return s.rec.At(extTick), false
		}
	}
	return t.nominalNS(extTick), true
}

func (t *Timeline) nominalNS(extTick int64) int64 {
	return extTick * 1_000_000_000 / 90_000
}
