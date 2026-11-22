// Package mp4 基于 go-mp4 检查分片 MP4（fMP4）：汇总每个 moof 的
// baseMediaDecodeTime 与关键帧间隔，识别时间戳不连续。
package mp4

import (
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"
)

// SampleFlagNonSync 是 trun/tfhd 中“非同步样本（非关键帧）”标记位。
const SampleFlagNonSync uint32 = 0x00010000

// FragmentSummary 是单个 moof 分片的检查摘要。
type FragmentSummary struct {
	Index            int    `json:"index"`
	BaseDecodeTime   uint64 `json:"base_decode_time"` // tfdt，视频时钟刻度
	SampleCount      uint32 `json:"sample_count"`
	KeyframeCount    int    `json:"keyframe_count"`
	HasKeyframe      bool   `json:"has_keyframe"`
	FirstSampleFlags uint32 `json:"first_sample_flags"`
}

// Report 是整段 fMP4 的检查结果（时间单位由外部 Clock 解释）。
type Report struct {
	Timescale       uint32            `json:"timescale"`
	Fragments       []FragmentSummary `json:"fragments"`
	KeyframeTimes   []uint64          `json:"keyframe_times"`
	Discontinuities []Discontinuity   `json:"discontinuities"`
	MaxKeyframeGap  uint64            `json:"max_keyframe_gap_ticks"`
}

// Discontinuity 描述相邻分片时间戳不连续。
type Discontinuity struct {
	PrevIndex int    `json:"prev_index"`
	NextIndex int    `json:"next_index"`
	Expected  uint64 `json:"expected_base"`
	Actual    uint64 `json:"actual_base"`
}

// fragState 累积单个 moof 内的 tfdt/tfhd/trun。
type fragState struct {
	tfdt *gomp4.Tfdt
	tfhd *gomp4.Tfhd
	trun []*gomp4.Trun
}

// Inspector 绑定视频时钟刻度（默认 90000）。
type Inspector struct {
	Timescale uint32
}

// Inspect 从 r（可 Seek）读取 fMP4 并产出检查报告。
func (in *Inspector) Inspect(r io.ReadSeeker) (*Report, error) {
	rep := &Report{Timescale: in.Timescale}
	var stack []*fragState
	var current *fragState

	handler := func(h *gomp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case gomp4.BoxTypeMoof():
			current = &fragState{}
			stack = append(stack, current)
		case gomp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if current != nil {
				current.tfdt = box.(*gomp4.Tfdt)
			}
		case gomp4.BoxTypeTfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if current != nil {
				current.tfhd = box.(*gomp4.Tfhd)
			}
		case gomp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if current != nil {
				current.trun = append(current.trun, box.(*gomp4.Trun))
			}
		}
		// 继续递归子 box。
		return h.Expand()
	}
	if _, err := gomp4.ReadBoxStructure(r, handler); err != nil {
		return nil, fmt.Errorf("mp4: read structure failed: %w", err)
	}
	if err := in.build(rep, stack); err != nil {
		return nil, err
	}
	return rep, nil
}

// build 把读取状态汇总为分片摘要、关键帧时间与不连续点。
func (in *Inspector) build(rep *Report, frags []*fragState) error {
	var prevEnd uint64
	havePrev := false
	for idx, fs := range frags {
		if fs.tfdt == nil {
			return fmt.Errorf("mp4: moof #%d missing tfdt", idx)
		}
		base := fs.tfdt.GetBaseMediaDecodeTime()
		sum := FragmentSummary{
			Index:          idx,
			BaseDecodeTime: base,
		}
		var sampleCount uint32
		var firstFlags uint32
		haveFirst := false
		for _, tr := range fs.trun {
			sampleCount += tr.SampleCount
			if !haveFirst && tr.SampleCount > 0 {
				// first_sample_flags 存在则用之，否则回落到 tfhd 默认标记。
				if tr.GetFlags()&0x000004 != 0 {
					firstFlags = tr.FirstSampleFlags
				} else if fs.tfhd != nil && fs.tfhd.GetFlags()&gomp4.TfhdDefaultSampleFlagsPresent != 0 {
					firstFlags = fs.tfhd.DefaultSampleFlags
				}
				haveFirst = true
			}
			for i, e := range tr.Entries {
				flags := sampleFlags(fs.tfhd, tr, i, e)
				if flags&SampleFlagNonSync == 0 {
					sum.KeyframeCount++
				}
			}
		}
		sum.SampleCount = sampleCount
		sum.FirstSampleFlags = firstFlags
		// 仅当存在明确的同步样本（未置 non-sync 位）时才认定关键帧。
		sum.HasKeyframe = sum.KeyframeCount > 0
		if sum.HasKeyframe {
			rep.KeyframeTimes = append(rep.KeyframeTimes, base)
		}
		rep.Fragments = append(rep.Fragments, sum)

		if havePrev && base != prevEnd {
			rep.Discontinuities = append(rep.Discontinuities, Discontinuity{
				PrevIndex: idx - 1,
				NextIndex: idx,
				Expected:  prevEnd,
				Actual:    base,
			})
		}
		// 估算分片占用的刻度长度（样本时长之和），作为下一片的期望起点。
		prevEnd = base + fragmentDuration(fs)
		havePrev = true
	}
	rep.MaxKeyframeGap = maxKeyframeGap(rep.KeyframeTimes)
	return nil
}

// sampleFlags 解析单个样本的有效标记：trun entry 优先，其次 first_sample_flags，
// 最后回落 tfhd 默认标记。
func sampleFlags(tfhd *gomp4.Tfhd, tr *gomp4.Trun, i int, e gomp4.TrunEntry) uint32 {
	if tr.GetFlags()&0x000400 != 0 { // sample_flags present
		return e.SampleFlags
	}
	if i == 0 && tr.GetFlags()&0x000004 != 0 {
		return tr.FirstSampleFlags
	}
	if tfhd != nil && tfhd.GetFlags()&gomp4.TfhdDefaultSampleFlagsPresent != 0 {
		return tfhd.DefaultSampleFlags
	}
	return 0
}

// fragmentDuration 汇总一个分片所有 trun 条目声明的样本时长；
// 若条目未带 duration，则无法估算，返回 0。
func fragmentDuration(fs *fragState) uint64 {
	var total uint64
	have := false
	for _, tr := range fs.trun {
		for _, e := range tr.Entries {
			if tr.GetFlags()&0x000100 != 0 {
				total += uint64(e.SampleDuration)
				have = true
			}
		}
	}
	if !have {
		return 0
	}
	return total
}

func maxKeyframeGap(times []uint64) uint64 {
	var maxGap uint64
	for i := 1; i < len(times); i++ {
		if g := times[i] - times[i-1]; g > maxGap {
			maxGap = g
		}
	}
	return maxGap
}
