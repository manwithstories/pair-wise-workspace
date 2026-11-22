// Package cut 按剪辑点与转场区间把样本流切分为片段。
//
// 剪辑点落在损坏/截断区间时，对应片段输出明确的 incomplete 状态，
// 流水线继续处理其余区间，局部故障不扩散。转场片段可同时引用多条
// 轨道上的样本区间，实现跨轨引用。
package cut

import (
	"fmt"

	"mediad/internal/clock"
	"mediad/internal/fault"
	"mediad/internal/model"
)

// msTimebase 是剪辑点输入时基：1000 tick / 秒。
var msTimebase = clock.MustNew(1000, 1)

// Point 是统一时间轴上的剪辑点（毫秒）。
type Point struct {
	AtMs       int64
	Transition bool // true 表示该点开启一个跨轨转场片段
}

// Split 把一条轨道的样本按剪辑点切分。
//
// 剪辑点毫秒值经整数有理数换算映射到轨道 tick；含不完整样本的片段
// 标记为 truncated，无样本覆盖的区间标记为 incomplete，其余片段保持
// ok。任何单个区间失败都不影响后续区间。
func Split(manifest *model.Manifest, track model.Track, sourceID string,
	points []Point) ([]model.Segment, []*fault.Error) {
	samples := trackSamples(manifest, track.ID)
	var segments []model.Segment
	var faults []*fault.Error
	if len(samples) == 0 {
		return nil, []*fault.Error{fault.Recoverable(fault.CodeIncomplete,
			"cut.Split", 0, 0, fmt.Errorf("track %s has no samples", track.ID))}
	}
	bounds := []int64{samples[0].PTS}
	for _, p := range points {
		tick, err := clock.Convert(p.AtMs, msTimebase, track.Timebase)
		if err != nil {
			faults = append(faults,
				fault.Fatal(fault.CodeInvalidInput, "cut.Convert", err))
			continue
		}
		bounds = append(bounds, tick)
	}
	last := samples[len(samples)-1]
	bounds = append(bounds, last.PTS+int64(last.Duration))

	for i := 0; i+1 < len(bounds); i++ {
		start, end := bounds[i], bounds[i+1]
		seg := model.Segment{
			ID:        fmt.Sprintf("%s-seg-%02d", track.ID, i+1),
			TrackID:   track.ID,
			StartTick: start,
			EndTick:   end,
			Status:    model.StatusOK,
		}
		var first, lastIdx uint32
		byteLen, count := 0, 0
		truncated := false
		for si := range samples {
			s := &samples[si]
			sEnd := s.PTS + int64(s.Duration)
			if sEnd <= start || s.PTS >= end {
				continue
			}
			if count == 0 {
				first = s.Index
			}
			lastIdx = s.Index
			count++
			byteLen += int(s.Size)
			if !s.Complete {
				truncated = true
			}
		}
		switch {
		case count == 0:
			seg.Status = model.StatusIncomplete
			faults = append(faults, fault.Recoverable(fault.CodeIncomplete,
				"cut.Split", start, end,
				fmt.Errorf("no samples cover [%d,%d) on %s", start, end, track.ID)))
		case byteLen > model.MaxSegmentBytes:
			seg.Status = model.StatusCorrupt
			faults = append(faults, fault.Fatal(fault.CodeTooLarge,
				"cut.Validate",
				fmt.Errorf("segment %s is %d bytes", seg.ID, byteLen)))
		case truncated:
			seg.Status = model.StatusTruncated
			seg.ByteLen = byteLen
			seg.Refs = []model.SourceRef{{
				SourceID: sourceID, TrackID: track.ID,
				FirstSample: first, LastSample: lastIdx,
				StartTick: start, EndTick: end,
			}}
			faults = append(faults, fault.Recoverable(fault.CodeIncomplete,
				"cut.Split", start, end,
				fmt.Errorf("segment %s contains truncated samples", seg.ID)))
		default:
			seg.ByteLen = byteLen
			seg.Refs = []model.SourceRef{{
				SourceID: sourceID, TrackID: track.ID,
				FirstSample: first, LastSample: lastIdx,
				StartTick: start, EndTick: end,
			}}
		}
		segments = append(segments, seg)
	}
	return segments, faults
}

// AttachTransition 把另一轨道在 [startMs,endMs) 内的样本区间附加到
// 转场片段，形成跨轨引用。区间无样本时返回可恢复错误且不改动片段。
func AttachTransition(seg *model.Segment, manifest *model.Manifest,
	other model.Track, otherSource string, startMs, endMs int64) error {
	startTick, err := clock.Convert(startMs, msTimebase, other.Timebase)
	if err != nil {
		return fault.Fatal(fault.CodeInvalidInput, "cut.Convert", err)
	}
	endTick, err := clock.Convert(endMs, msTimebase, other.Timebase)
	if err != nil {
		return fault.Fatal(fault.CodeInvalidInput, "cut.Convert", err)
	}
	var first, last uint32
	hits := 0
	for i := range manifest.Samples {
		s := &manifest.Samples[i]
		if s.TrackID != other.ID {
			continue
		}
		sEnd := s.PTS + int64(s.Duration)
		if sEnd <= startTick || s.PTS >= endTick {
			continue
		}
		if hits == 0 {
			first = s.Index
		}
		last = s.Index
		hits++
	}
	if hits == 0 {
		return fault.Recoverable(fault.CodeIncomplete, "cut.AttachTransition",
			startTick, endTick, fmt.Errorf("transition has no samples on %s", other.ID))
	}
	seg.Refs = append(seg.Refs, model.SourceRef{
		SourceID: otherSource, TrackID: other.ID,
		FirstSample: first, LastSample: last,
		StartTick: startTick, EndTick: endTick,
	})
	return nil
}

func trackSamples(m *model.Manifest, trackID string) []model.Sample {
	var out []model.Sample
	for i := range m.Samples {
		if m.Samples[i].TrackID == trackID {
			out = append(out, m.Samples[i])
		}
	}
	return out
}
