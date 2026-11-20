// Package manifest 用 go-mp4 读取本地 MP4 轨道元数据，
// 生成时间对齐清单及码率统计，并执行验证。
package manifest

import (
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediamux/media"
)

// Track 是一条 MP4 轨道的元数据，时长已换算到统一时基。
type Track struct {
	ID        uint32
	Codec     string
	Timescale uint32
	Duration  media.Ticks
	Samples   int
	Bitrate   int64 // 按样本总字节估算的码率 bps
}

// Gap 是交付流中的一段缺口（统一时基）。
type Gap struct {
	Track string
	Start media.Ticks
	End   media.Ticks
}

// SubRange 是一条字幕区间（统一时基）。
type SubRange struct {
	Start media.Ticks
	End   media.Ticks
}

// Check 是一项验证结果。
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Manifest 是时间对齐清单：轨道、缺口、字幕区间、目标/实际码率与校验状态。
type Manifest struct {
	Tracks    []Track
	Gaps      []Gap
	Subtitles []SubRange
	TargetBps int64
	ActualBps int64
	Checks    []Check
}

// ReadMP4 用 go-mp4 读取本地 MP4 的轨道元数据。
func ReadMP4(path string) ([]Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.New(media.StageManifest, "mp4", -1, false, "打开失败: %v", err)
	}
	defer f.Close()
	info, err := mp4.Probe(f)
	if err != nil {
		return nil, media.New(media.StageManifest, "mp4", -1, false, "Probe 失败: %v", err)
	}
	var tracks []Track
	for _, t := range info.Tracks {
		codec := "unknown"
		switch t.Codec {
		case mp4.CodecAVC1:
			codec = "avc1"
		case mp4.CodecMP4A:
			codec = "mp4a"
		}
		dur := media.Convert(media.Ticks(t.Duration),
			media.Timebase{Num: 1, Den: int64(t.Timescale)}, media.MediaTimebase)
		var bytes uint64
		for _, s := range t.Samples {
			bytes += uint64(s.Size)
		}
		var bps int64
		if secs := media.MediaTimebase.Seconds(dur); secs > 0 {
			bps = int64(float64(bytes*8) / secs)
		}
		tracks = append(tracks, Track{
			ID: t.TrackID, Codec: codec, Timescale: t.Timescale,
			Duration: dur, Samples: len(t.Samples), Bitrate: bps,
		})
	}
	return tracks, nil
}

// AlignmentError 返回字幕区间超出轨道覆盖范围的最大偏差（统一时基）。
func (m *Manifest) AlignmentError() media.Ticks {
	var end media.Ticks
	for _, t := range m.Tracks {
		if t.Duration > end {
			end = t.Duration
		}
	}
	var maxErr media.Ticks
	for _, s := range m.Subtitles {
		if s.Start < 0 && -s.Start > maxErr {
			maxErr = -s.Start
		}
		if s.End > end && s.End-end > maxErr {
			maxErr = s.End - end
		}
	}
	return maxErr
}

// Validate 检查时间单调性、轨道覆盖与清单时间对齐（maxAlign 通常为 1 帧时长），
// 结果写入 m.Checks 并返回。
func (m *Manifest) Validate(maxAlign media.Ticks) []Check {
	var checks []Check

	mono := true
	for i := 1; i < len(m.Subtitles); i++ {
		if m.Subtitles[i].Start < m.Subtitles[i-1].Start {
			mono = false
		}
	}
	checks = append(checks, Check{Name: "subtitle_monotonic", OK: mono,
		Detail: fmt.Sprintf("ranges=%d", len(m.Subtitles))})

	cov := len(m.Tracks) > 0
	for _, t := range m.Tracks {
		if t.Duration <= 0 {
			cov = false
		}
	}
	checks = append(checks, Check{Name: "track_coverage", OK: cov,
		Detail: fmt.Sprintf("tracks=%d", len(m.Tracks))})

	align := m.AlignmentError()
	checks = append(checks, Check{Name: "manifest_alignment", OK: align <= maxAlign,
		Detail: fmt.Sprintf("max_err_ms=%d budget_ms=%d", media.Millis(align), media.Millis(maxAlign))})

	bitrateOK := true
	if m.TargetBps > 0 {
		dev := float64(m.ActualBps-m.TargetBps) / float64(m.TargetBps)
		if dev < 0 {
			dev = -dev
		}
		bitrateOK = dev <= 0.05
	}
	checks = append(checks, Check{Name: "bitrate_deviation", OK: bitrateOK,
		Detail: fmt.Sprintf("target=%d actual=%d", m.TargetBps, m.ActualBps)})

	m.Checks = checks
	return checks
}

// AllOK 报告全部校验是否通过。
func (m *Manifest) AllOK() bool {
	for _, c := range m.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}
