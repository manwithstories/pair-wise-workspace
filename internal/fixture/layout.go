package fixture

import (
	"fmt"

	"mediarepair/internal/model"
)

// laySegment 是夹具时间线上的一段素材。
type laySegment struct {
	id           string
	trackID      uint32
	first        int
	count        int
	startTick    uint64 // 逻辑时间线起点（轨道时钟）
	durationTick uint64
	transition   bool
	from, to     string
}

// layout 保存生成的样本计划与片段划分。
type layout struct {
	video    []samplePlan
	audio    []samplePlan
	segs     []laySegment
	inflated []bool // 该样本的 delta 含注入量（剪辑跳变/回绕），不计入逻辑时间
}

const (
	// videoBaseDelta 是 30fps@90kHz 的一帧时长。
	videoBaseDelta uint32 = 3000
	// audioBaseDelta 是 AAC 48kHz 的帧长。
	audioBaseDelta uint32 = 1024
	videoPayload          = 64
	audioPayload          = 32
)

// videoSegments 把视频样本划分为 3 个剪辑与 1 个转场；样本过少时退化为单剪辑。
func videoSegments(n int) []laySegment {
	if n < 10 {
		return []laySegment{{id: "v-clip-1", trackID: 1, first: 0, count: n}}
	}
	n1 := n * 40 / 100
	nt := n / 10
	n2 := n * 25 / 100
	n3 := n - n1 - nt - n2
	return []laySegment{
		{id: "v-clip-1", trackID: 1, first: 0, count: n1},
		{id: "v-trans-12", trackID: 1, first: n1, count: nt, transition: true, from: "v-clip-1", to: "v-clip-2"},
		{id: "v-clip-2", trackID: 1, first: n1 + nt, count: n2},
		{id: "v-clip-3", trackID: 1, first: n1 + nt + n2, count: n3},
	}
}

// audioSegments 把音频样本划分为 1-2 个剪辑。
func audioSegments(m int) []laySegment {
	if m == 0 {
		return nil
	}
	if m < 2 {
		return []laySegment{{id: "a-clip-1", trackID: 2, first: 0, count: m}}
	}
	h := m / 2
	return []laySegment{
		{id: "a-clip-1", trackID: 2, first: 0, count: h},
		{id: "a-clip-2", trackID: 2, first: h, count: m - h},
	}
}

// buildVideoDeltas 生成视频样本时长，并返回按样本索引的“注入量”：
// 当 segs 在 index 处是新片段起点时，index-1 的 delta 被额外拉长，以制造
// 回绕（PatternWrap）或剪辑跳变（PatternClipJump）。注入量不计入逻辑时间线。
func buildVideoDeltas(spec Spec, segs []laySegment) ([]uint32, []uint64) {
	n := spec.VideoSamples
	deltas := make([]uint32, n)
	injected := make([]uint64, n)
	for i := range deltas {
		deltas[i] = videoBaseDelta
	}
	switch spec.Pattern {
	case PatternClipJump:
		for _, sg := range segs[1:] {
			span := uint64(sg.count) * uint64(videoBaseDelta)
			g := span/4 + uint64(videoBaseDelta)
			deltas[sg.first-1] += uint32(g)
			injected[sg.first-1] += g
		}
	case PatternWrap:
		ws := n / 2
		before := uint64(ws) * uint64(videoBaseDelta)
		if before < uint64(1)<<32 {
			g := uint64(1)<<32 + uint64(videoBaseDelta) - before
			deltas[ws-1] = uint32(g)
			injected[ws-1] = g - uint64(videoBaseDelta)
		}
	case PatternDrift:
		for i := range deltas {
			if i%2 == 0 {
				deltas[i]++
			} else {
				deltas[i]--
			}
		}
	}
	return deltas, injected
}

// logicalTicks 由原始 delta 减去注入量，得到连续逻辑时间线的累计值。
func logicalTicks(deltas []uint32, injected []uint64) []uint64 {
	logical := make([]uint64, len(deltas)+1)
	for i, d := range deltas {
		logical[i+1] = logical[i] + uint64(d) - injected[i]
	}
	return logical
}

// buildLayout 计算样本计划、片段划分与 sidecar 时间线。
func buildLayout(spec Spec) (*model.Timeline, *layout, error) {
	vsegs := videoSegments(spec.VideoSamples)
	deltas, injected := buildVideoDeltas(spec, vsegs)
	logical := logicalTicks(deltas, injected)

	video := make([]samplePlan, len(deltas))
	for i, d := range deltas {
		video[i] = samplePlan{delta: d, size: videoPayload, keyframe: i%10 == 0}
	}

	l := &layout{video: video, inflated: make([]bool, len(deltas))}
	for i := range injected {
		l.inflated[i] = injected[i] > 0
	}
	tl := &model.Timeline{RTPTrackID: 1}
	tl.Tracks = append(tl.Tracks,
		model.Track{ID: 1, Kind: model.TrackVideo, Timescale: spec.VideoTimescale, Name: "cam-team"},
		model.Track{ID: 2, Kind: model.TrackAudio, Timescale: spec.AudioTimescale, Name: "mic-team"},
	)

	for idx, sg := range vsegs {
		var end uint64
		if idx+1 < len(vsegs) {
			end = logical[vsegs[idx+1].first]
		} else {
			end = logical[len(deltas)]
		}
		sg.startTick = logical[sg.first]
		sg.durationTick = end - sg.startTick
		l.segs = append(l.segs, sg)
		switch {
		case sg.transition:
			tl.Transitions = append(tl.Transitions, model.Transition{
				ID: sg.id, TrackID: sg.trackID, FromClipID: sg.from, ToClipID: sg.to,
				StartTick: sg.startTick, DurationTick: sg.durationTick,
				FirstSample: uint64(sg.first), SampleCount: uint64(sg.count),
			})
		default:
			tl.Clips = append(tl.Clips, model.Clip{
				ID: sg.id, TrackID: sg.trackID, Index: idx,
				StartTick: sg.startTick, DurationTick: sg.durationTick,
				FirstSample: uint64(sg.first), SampleCount: uint64(sg.count),
			})
		}
	}

	// 音频：等长帧，逻辑时间线与原始累计一致。
	asegs := audioSegments(spec.AudioSamples)
	audio := make([]samplePlan, spec.AudioSamples)
	acum := make([]uint64, len(audio)+1)
	for i := range audio {
		audio[i] = samplePlan{delta: audioBaseDelta, size: audioPayload, keyframe: true}
		acum[i+1] = acum[i] + uint64(audioBaseDelta)
	}
	l.audio = audio
	for idx, sg := range asegs {
		var end uint64
		if idx+1 < len(asegs) {
			end = acum[asegs[idx+1].first]
		} else {
			end = acum[len(audio)]
		}
		sg.startTick = acum[sg.first]
		sg.durationTick = end - sg.startTick
		l.segs = append(l.segs, sg)
		tl.Clips = append(tl.Clips, model.Clip{
			ID: sg.id, TrackID: sg.trackID, Index: idx,
			StartTick: sg.startTick, DurationTick: sg.durationTick,
			FirstSample: uint64(sg.first), SampleCount: uint64(sg.count),
		})
	}

	if err := tl.Validate(); err != nil {
		return nil, nil, fmt.Errorf("fixture: 生成的时间线非法: %w", err)
	}
	totalBytes := len(video)*videoPayload + len(audio)*audioPayload
	if totalBytes > maxFixtureBytes {
		return nil, nil, fmt.Errorf("fixture: 样本负载总量 %d 超出上限 %d", totalBytes, maxFixtureBytes)
	}
	return tl, l, nil
}
