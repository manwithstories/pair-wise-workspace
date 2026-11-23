// Package sync 依据关键帧边界与音频时钟对齐音视频，
// 输出清单起止时间与各轨道时间基准。
package sync

import (
	"errors"

	"mediakernel/internal/clock"
)

// TrackTime 是一条轨道的观测时间线。
type TrackTime struct {
	Track uint64
	Type  int // 1=视频 2=音频
	Base  clock.Nanos
	End   clock.Nanos
	Keys  []clock.Nanos // 关键帧边界（视频）
}

// Correction 记录一次漂移修正。
type Correction struct {
	Track  uint64
	From   clock.Nanos
	To     clock.Nanos
	Reason string
}

// Alignment 是对齐结果，直接映射到清单字段。
type Alignment struct {
	Start       clock.Nanos
	End         clock.Nanos
	Bases       map[uint64]clock.Nanos
	Drift       clock.Nanos // 视频相对音频的漂移
	Corrections []Correction
}

// Policy 控制对齐容差。
type Policy struct {
	Tolerance clock.Nanos // 允许的最大漂移
}

// Align 以音频时钟为基准；视频漂移超限时吸附到
// 不超过音频起点的最近关键帧边界。
func (p Policy) Align(tracks []TrackTime) (Alignment, error) {
	if len(tracks) == 0 {
		return Alignment{}, errors.New("sync: no tracks")
	}
	al := Alignment{Bases: make(map[uint64]clock.Nanos)}
	var audio, video *TrackTime
	for i := range tracks {
		tr := &tracks[i]
		al.Bases[tr.Track] = tr.Base
		if i == 0 || tr.Base < al.Start {
			al.Start = tr.Base
		}
		if i == 0 || tr.End > al.End {
			al.End = tr.End
		}
		switch tr.Type {
		case 1:
			video = tr
		case 2:
			audio = tr
		}
	}
	if audio == nil || video == nil {
		return al, nil
	}
	al.Drift = video.Base - audio.Base
	if al.Drift < 0 {
		if -al.Drift <= p.Tolerance {
			return al, nil
		}
	} else if al.Drift <= p.Tolerance {
		return al, nil
	}
	// 吸附到不超过音频起点的最近关键帧；没有则取最早关键帧。
	target := video.Keys[0]
	for _, k := range video.Keys {
		if k <= audio.Base {
			target = k
		} else {
			break
		}
	}
	al.Corrections = append(al.Corrections, Correction{
		Track:  video.Track,
		From:   video.Base,
		To:     target,
		Reason: "keyframe snap to audio clock",
	})
	al.Bases[video.Track] = target
	al.Start = target
	if audio.Base < target {
		al.Start = audio.Base
	}
	return al, nil
}
