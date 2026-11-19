package parse

import (
	"encoding/json"
	"fmt"
	"os"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
)

// TimelineVersion 是 sidecar 时间线文件的必需标记。
const TimelineVersion = "mediarepair-timeline/v1"

type timelineJSON struct {
	Version     string `json:"version"`
	Tracks      []struct {
		ID        uint32 `json:"id"`
		Kind      string `json:"kind"`
		Timescale uint32 `json:"timescale"`
		Name      string `json:"name"`
	} `json:"tracks"`
	Clips []struct {
		ID           string `json:"id"`
		TrackID      uint32 `json:"track_id"`
		Index        int    `json:"index"`
		StartTick    uint64 `json:"start_tick"`
		DurationTick uint64 `json:"duration_tick"`
	} `json:"clips"`
	Transitions []struct {
		ID           string `json:"id"`
		TrackID      uint32 `json:"track_id"`
		FromClipID   string `json:"from_clip_id"`
		ToClipID     string `json:"to_clip_id"`
		StartTick    uint64 `json:"start_tick"`
		DurationTick uint64 `json:"duration_tick"`
	} `json:"transitions"`
}

// LoadTimeline 读取时间线 sidecar JSON，校验必需标记与时间基准上限。
func LoadTimeline(path string) (*model.Timeline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Wrap(fault.StageIO, 0, -1, false, "读取时间线文件失败", err)
	}
	if len(data) > 1<<20 {
		return nil, fault.New(fault.StageParse, 0, 0, false, "时间线文件超出 1MiB 上限")
	}
	var tj timelineJSON
	if err := json.Unmarshal(data, &tj); err != nil {
		return nil, fault.Wrap(fault.StageParse, 0, 0, false, "时间线 JSON 解析失败", err)
	}
	if tj.Version != TimelineVersion {
		return nil, fault.New(fault.StageParse, 0, 0, false,
			fmt.Sprintf("未知必需标记 version=%q（期望 %q）", tj.Version, TimelineVersion))
	}
	tl := &model.Timeline{}
	for _, t := range tj.Tracks {
		tl.Tracks = append(tl.Tracks, model.Track{
			ID: t.ID, Kind: model.TrackKind(t.Kind), Timescale: t.Timescale, Name: t.Name,
		})
	}
	for _, c := range tj.Clips {
		tl.Clips = append(tl.Clips, model.Clip{
			ID: c.ID, TrackID: c.TrackID, Index: c.Index,
			StartTick: c.StartTick, DurationTick: c.DurationTick,
		})
	}
	for _, tr := range tj.Transitions {
		tl.Transitions = append(tl.Transitions, model.Transition{
			ID: tr.ID, TrackID: tr.TrackID, FromClipID: tr.FromClipID, ToClipID: tr.ToClipID,
			StartTick: tr.StartTick, DurationTick: tr.DurationTick,
		})
	}
	if err := tl.Validate(); err != nil {
		return nil, fault.Wrap(fault.StageParse, 0, 0, false, "时间线校验失败", err)
	}
	return tl, nil
}
