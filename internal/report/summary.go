// Package report 汇总各子系统的解析结果，产出终端输出所需的稳定指标集合。
package report

import (
	"sort"

	"relaycheck/internal/model"
)

// Summary 是一次分析运行的终端可打印指标。
type Summary struct {
	Segments          int     `json:"segments"`
	Samples           int     `json:"samples"`
	FirstBad          int64   `json:"first_bad"`            // 全输入首个错误字节偏移，-1 表示无
	FirstBadSegment   int     `json:"first_bad_segment"`    // 首个错误所在分片，-1 表示无
	OffsetDeltaBytes  int64   `json:"offset_delta_bytes"`    // 首错偏移与截断/损坏参考点的偏差
	KeyframeGapMS     []int64 `json:"keyframe_gap_ms"`       // 关键帧间隔（毫秒）
	MemoryPeakBytes   int64   `json:"memory_peak_bytes"`     // 内存峰值
	ResumeMS          int64   `json:"resume_ms"`             // 续跑耗时，首次运行为 0
	SHA256            string  `json:"sha256"`                // 输出校验和
	FaultCount        int     `json:"fault_count"`
	Faults            []model.Fault `json:"faults,omitempty"`
}

// Builder 增量汇总流水线记录。
type Builder struct {
	sum       Summary
	syncTimes []model.Rat
	faults    []model.Fault
}

// NewBuilder 创建汇总构建器，firstBad 初始为 -1。
func NewBuilder() *Builder {
	return &Builder{sum: Summary{FirstBad: -1, FirstBadSegment: -1}}
}

// AddFragment 纳入一个分片的解析结果。
func (b *Builder) AddFragment(rep model.FragmentReport) {
	b.sum.Segments++
	b.sum.Samples += len(rep.Samples)
	b.syncTimes = append(b.syncTimes, rep.SyncTimes...)
	b.absorbFaults(rep.Faults)
	if rep.FirstBadOff >= 0 {
		b.noteBad(rep.Seq, rep.FirstBadOff)
	}
}

// AddRTP 纳入一个 RTP 诊断结果。
func (b *Builder) AddRTP(rep model.RTPReport) {
	b.absorbFaults(rep.Faults)
}

// AddManifest 纳入清单互查结果。
func (b *Builder) AddManifest(rep model.ManifestReport) {
	b.absorbFaults(rep.Faults)
}

func (b *Builder) absorbFaults(faults []model.Fault) {
	b.faults = append(b.faults, faults...)
}

func (b *Builder) noteBad(seg int, off int64) {
	if b.sum.FirstBad < 0 || off < b.sum.FirstBad {
		b.sum.FirstBad = off
		b.sum.FirstBadSegment = seg
	}
}

// Finalize 计算关键帧间隔、故障排序与稳定输出，memPeak/resumeMS/sha 由调用方补齐。
func (b *Builder) Finalize(memPeak, resumeMS int64, sha string) Summary {
	sort.SliceStable(b.syncTimes, func(i, j int) bool {
		return b.syncTimes[i].Cmp(b.syncTimes[j]) < 0
	})
	if len(b.syncTimes) >= 2 {
		for i := 1; i < len(b.syncTimes); i++ {
			gap := b.syncTimes[i].Sub(b.syncTimes[i-1])
			if gap.IsValid() {
				b.sum.KeyframeGapMS = append(b.sum.KeyframeGapMS, gap.RoundMillis())
			}
		}
	}
	sort.SliceStable(b.faults, func(i, j int) bool {
		if b.faults[i].Segment != b.faults[j].Segment {
			return b.faults[i].Segment < b.faults[j].Segment
		}
		return b.faults[i].Offset < b.faults[j].Offset
	})
	b.sum.Faults = b.faults
	b.sum.FaultCount = len(b.faults)
	b.sum.MemoryPeakBytes = memPeak
	b.sum.ResumeMS = resumeMS
	b.sum.SHA256 = sha
	return b.sum
}

// SetOffsetDelta 由编排层根据夹具参考点回填偏差（验收要求 <=16 字节）。
func (b *Builder) SetOffsetDelta(delta int64) { b.sum.OffsetDeltaBytes = delta }

// Faults 返回已收集故障的副本。
func (b *Builder) Faults() []model.Fault {
	out := make([]model.Fault, len(b.faults))
	copy(out, b.faults)
	return out
}
