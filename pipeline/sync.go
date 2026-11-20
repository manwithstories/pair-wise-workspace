package pipeline

import (
	"sort"

	"mediamux/media"
)

// SyncReport 是字幕与音视频时间线的对齐统计。
type SyncReport struct {
	Count      int         // 参与对齐的字幕事件数
	MaxErr     media.Ticks // 最大对齐误差
	AvgErr     media.Ticks // 平均对齐误差
	P95        media.Ticks // P95 对齐误差
	Incomplete bool        // 缺少参考轨或时间戳倒退时为 true
	Reasons    []string    // 不完整原因
}

// Aligner 按统一媒体时间基准对齐字幕事件与参考轨（音视频包）。
// 输入规模有界（包与记录均有上限），参考点在内存中累积用于最终统计。
type Aligner struct {
	refs    []media.Ticks
	subs    []media.Ticks
	lastRef media.Ticks
	hasRef  bool
	regress bool
}

// NewAligner 构造对齐器。
func NewAligner() *Aligner { return &Aligner{} }

// ObserveReference 记录一个参考轨时间点（音视频包的媒体时间）。
// 时间戳倒退会被标记，最终报告记为不完整。
func (a *Aligner) ObserveReference(t media.Ticks) {
	if a.hasRef && t < a.lastRef {
		a.regress = true
	}
	a.lastRef = t
	a.hasRef = true
	a.refs = append(a.refs, t)
}

// ObserveSubtitle 记录一个字幕事件的开始时间。
func (a *Aligner) ObserveSubtitle(start media.Ticks) {
	a.subs = append(a.subs, start)
}

// Report 计算对齐误差统计：每条字幕取与其开始时间最近的参考点。
func (a *Aligner) Report() SyncReport {
	rep := SyncReport{Count: len(a.subs)}
	if !a.hasRef {
		rep.Incomplete = true
		rep.Reasons = append(rep.Reasons, "缺少参考轨")
	}
	if a.regress {
		rep.Incomplete = true
		rep.Reasons = append(rep.Reasons, "参考轨时间戳倒退")
	}
	if len(a.subs) == 0 {
		rep.Incomplete = true
		rep.Reasons = append(rep.Reasons, "无字幕事件")
		return rep
	}
	if !a.hasRef {
		return rep
	}
	refs := append([]media.Ticks(nil), a.refs...)
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	errs := make([]media.Ticks, 0, len(a.subs))
	for _, s := range a.subs {
		errs = append(errs, absTick(s - nearest(refs, s)))
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i] < errs[j] })
	var sum media.Ticks
	for _, e := range errs {
		sum += e
	}
	rep.MaxErr = errs[len(errs)-1]
	rep.AvgErr = sum / media.Ticks(len(errs))
	p95 := (95*len(errs) + 99) / 100 // ceil(0.95*n)
	rep.P95 = errs[p95-1]
	return rep
}

// nearest 在已排序的 refs 中找离 t 最近的参考点。
func nearest(refs []media.Ticks, t media.Ticks) media.Ticks {
	i := sort.Search(len(refs), func(i int) bool { return refs[i] >= t })
	if i == 0 {
		return refs[0]
	}
	if i == len(refs) {
		return refs[len(refs)-1]
	}
	if t-refs[i-1] <= refs[i]-t {
		return refs[i-1]
	}
	return refs[i]
}

func absTick(t media.Ticks) media.Ticks {
	if t < 0 {
		return -t
	}
	return t
}
