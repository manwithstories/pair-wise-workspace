// Package timeline 把适配器产出的多轨、异构时基时间戳归一到一条
// 以 MediaTime（微秒）为单位的统一时间轴。
//
// 关键不变量：
//   - 全内核只有这一条时间轴，禁止各轨独立累计时间；
//   - 负起点、非零起点通过每轨 Offset 平移到统一轴；
//   - 回绕必须在适配器层先展开，本层只接受单调（允许相等）的刻度；
//   - 可变帧率由 packet 自带 DurationTicks 表达，不假设固定帧间隔。
package timeline

import (
	"sort"

	"archivemedia/internal/adapter"
	"archivemedia/internal/model"
)

// NormalizedFrame 是统一时间轴上的一帧/一个采样块。
type NormalizedFrame struct {
	TrackIndex  int
	Start       model.MediaTime // 统一轴起点（微秒，可为负）
	End         model.MediaTime
	Duration    model.Duration
	KeyFrame    bool
	ContentHash uint64
	PayloadSize int
}

// NormalizedTrack 保留轨道元数据与其在统一轴上的全部帧。
type NormalizedTrack struct {
	Track  adapter.Track
	Offset model.MediaTime // 归一零点平移量
	Frames []NormalizedFrame
}

// Timeline 是归一结果。
type Timeline struct {
	SourceID       string
	AdapterName    string
	AdapterVersion string
	Tracks         []NormalizedTrack
	// Span 是统一轴覆盖范围（含负起点）。
	SpanStart model.MediaTime
	SpanEnd   model.MediaTime
	Wraps     []adapter.WrapEvent
}

// Normalize 执行多轨归一。
func Normalize(pm *adapter.ParsedMedia) (*Timeline, error) {
	if pm == nil {
		return nil, model.NewError(model.KindInvalidArgument, "timeline: nil parsed media")
	}
	tl := &Timeline{
		SourceID:       pm.SourceID,
		AdapterName:    pm.AdapterName,
		AdapterVersion: pm.AdapterVersion,
		Wraps:          pm.Wraps,
	}
	for _, tr := range pm.Tracks {
		nt := NormalizedTrack{Track: tr}
		tl.Tracks = append(tl.Tracks, nt)
	}
	// 按轨分组 packet，保持组内原始顺序（适配器已展开回绕）。
	byTrack := map[int][]adapter.FramePacket{}
	for _, pkt := range pm.Packets {
		if pkt.TrackIndex < 0 || pkt.TrackIndex >= len(pm.Tracks) {
			return nil, model.NewError(model.KindCorruptMedia,
				"timeline: packet track %d out of range", pkt.TrackIndex)
		}
		byTrack[pkt.TrackIndex] = append(byTrack[pkt.TrackIndex], pkt)
	}
	for ti := range tl.Tracks {
		frames, offset, err := normalizeTrack(tl.Tracks[ti].Track, byTrack[ti])
		if err != nil {
			return nil, err
		}
		tl.Tracks[ti].Frames = frames
		tl.Tracks[ti].Offset = offset
		for _, fr := range frames {
			tl.SpanStart = model.MinTime(tl.SpanStart, fr.Start)
			tl.SpanEnd = model.MaxTime(tl.SpanEnd, fr.End)
		}
	}
	return tl, nil
}

// normalizeTrack 归一单轨：轨道刻度 -> 统一微秒轴。
//
// 不做“把首帧平移到 0”的处理：负起点与非零起点是素材的真实时间属性，
// 必须在统一轴上原样保留；Offset 仅记录首帧时间供溯源与对齐核验。
// 可变帧率由相邻 packet 起点推导帧时长，不假设固定帧间隔。
func normalizeTrack(tr adapter.Track, pkts []adapter.FramePacket) ([]NormalizedFrame, model.MediaTime, error) {
	if len(pkts) == 0 {
		return nil, 0, nil
	}
	offset := model.TicksToTime(pkts[0].StartTicks, tr.TimeBase)
	frames := make([]NormalizedFrame, 0, len(pkts))
	for i, pkt := range pkts {
		start := model.TicksToTime(pkt.StartTicks, tr.TimeBase)
		var end model.MediaTime
		switch {
		case pkt.DurationTicks > 0:
			end = model.TicksToTime(pkt.StartTicks+pkt.DurationTicks, tr.TimeBase)
		case i+1 < len(pkts):
			end = model.TicksToTime(pkts[i+1].StartTicks, tr.TimeBase) // VFR
		default:
			end = start // 末帧无时长信息时退化为点
		}
		if end < start {
			return nil, 0, model.NewError(model.KindCorruptMedia,
				"timeline: track %d frame %d ends before it starts", tr.Index, i)
		}
		nf := NormalizedFrame{
			TrackIndex:  tr.Index,
			Start:       start,
			End:         end,
			Duration:    model.DurationOf(start, end),
			KeyFrame:    pkt.KeyFrame,
			ContentHash: pkt.ContentHash,
			PayloadSize: pkt.PayloadSize,
		}
		// 单调性校验：若适配器未展开回绕，倒退时间戳在此暴露。
		if i > 0 && nf.Start.Before(frames[i-1].Start) {
			return nil, 0, model.NewError(model.KindCorruptMedia,
				"timeline: non-monotonic timestamp on track %d frame %d", tr.Index, i)
		}
		frames = append(frames, nf)
	}
	return frames, offset, nil
}

// FramesInWindow 返回某轨与 [lo, hi) 相交的帧（统一轴坐标）。
func (tl *Timeline) FramesInWindow(trackIdx int, lo, hi model.MediaTime) []NormalizedFrame {
	if trackIdx < 0 || trackIdx >= len(tl.Tracks) {
		return nil
	}
	var out []NormalizedFrame
	for _, f := range tl.Tracks[trackIdx].Frames {
		if f.End.After(lo) && f.Start.Before(hi) {
			out = append(out, f)
		}
	}
	return out
}

// AllFrames 按统一轴起点排序返回所有轨的帧（轨序为次序键，保证确定性）。
func (tl *Timeline) AllFrames() []NormalizedFrame {
	var all []NormalizedFrame
	for ti := range tl.Tracks {
		all = append(all, tl.Tracks[ti].Frames...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Start.Equals(all[j].Start) {
			return all[i].Start.Before(all[j].Start)
		}
		return all[i].TrackIndex < all[j].TrackIndex
	})
	return all
}

// TotalDuration 返回统一轴总时长。
func (tl *Timeline) TotalDuration() model.Duration {
	return model.DurationOf(tl.SpanStart, tl.SpanEnd)
}
