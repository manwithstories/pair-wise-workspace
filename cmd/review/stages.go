package main

import (
	"fmt"
	"os"
	"path/filepath"

	"review/internal/clock"
	"review/internal/fault"
	"review/internal/fixture"
	"review/internal/journal"
	"review/internal/render"
	"review/internal/timeline"
)

// stageMix 统一时基合成：多机位视频/音频/HLS 节目轨。
func (st *pipeState) stageMix(dir string, faults *[]*fault.Error) (string, error) {
	want := len(st.cams) * fixture.FramesPerCamera
	if len(st.payloads) != want || len(st.audio) != want {
		return "", fault.Newf(fault.KindPacketLoss, "recover", int64(len(st.payloads)), false,
			"payload count %d/%d, want %d", len(st.payloads), len(st.audio), want)
	}
	base, err := clock.NewBase(fixture.VideoTimescale, fixture.AudioTimescale)
	if err != nil {
		return "", err
	}
	frameTicks := base.Timescale / 30 // 每帧基准 tick
	var videoTracks, audioTracks []timeline.Track
	ref := 0
	for ci, cm := range st.cams {
		vt := timeline.Track{ID: cm.id + "/video", Kind: timeline.Video}
		at := timeline.Track{ID: cm.id + "/audio", Kind: timeline.Audio}
		for _, seg := range cm.hiSegs {
			for _, s := range seg.Samples {
				tick, err := base.Map(s.Time, fixture.VideoTimescale)
				if err != nil {
					return "", err
				}
				vt.Samples = append(vt.Samples, timeline.Sample{Tick: tick, Dur: frameTicks, Ref: ref})
				ref++
			}
		}
		// 截断缺失的尾帧：以 mid 档补齐（故障转移已在 recover 完成，此处补时轴）
		for len(vt.Samples) < fixture.FramesPerCamera {
			i := len(vt.Samples)
			seg := cm.midSegs[i/fixture.FramesPerSeg]
			s := seg.Samples[i%fixture.FramesPerSeg]
			tick, err := base.Map(s.Time, fixture.VideoTimescale)
			if err != nil {
				return "", err
			}
			vt.Samples = append(vt.Samples, timeline.Sample{Tick: tick, Dur: frameTicks, Ref: ref})
			ref++
		}
		for _, seg := range cm.audSegs {
			for _, s := range seg.Samples {
				tick, err := base.Map(s.Time, fixture.AudioTimescale)
				if err != nil {
					return "", err
				}
				at.Samples = append(at.Samples, timeline.Sample{Tick: tick, Dur: frameTicks, Ref: 0})
			}
		}
		videoTracks = append(videoTracks, vt)
		audioTracks = append(audioTracks, at)
		_ = ci
	}
	// HLS 节目轨：discontinuity 处重置时基，处理断点后时间戳回退
	hlsTrack := timeline.Track{ID: "hls/program", Kind: timeline.Video}
	tracker := clock.NewTracker(base, fixture.VideoTimescale, 1<<32)
	var chainEnd int64
	for i, seg := range st.hlsSegs {
		if st.hls.Segments[i].Discontinuity {
			tracker.Reset(chainEnd)
		}
		for _, s := range seg.Samples {
			tick, err := tracker.Map(s.Time)
			if err != nil {
				*faults = append(*faults, fault.Newf(fault.KindTimestampRollback,
					"hls/program", s.Time, true, "%v", err))
				continue
			}
			hlsTrack.Samples = append(hlsTrack.Samples, timeline.Sample{Tick: tick, Dur: frameTicks, Ref: 0})
			if d := tick + frameTicks; d > chainEnd {
				chainEnd = d
			}
		}
	}
	videoTracks = append(videoTracks, hlsTrack)
	vt := timeline.Mix(videoTracks, frameTicks, base)
	at := timeline.Mix(audioTracks, frameTicks, base)
	st.tl = vt
	*faults = append(*faults, vt.GapFaults()...)
	*faults = append(*faults, at.GapFaults()...)
	summary := fmt.Sprintf("slots=%d sync_max_ms=%.2f gaps=%d hls_samples=%d\n",
		len(vt.Slots), vt.SyncMaxMs, len(vt.Gaps)+len(at.Gaps), len(hlsTrack.Samples))
	if err := os.WriteFile(filepath.Join(dir, "mix-summary.txt"), []byte(summary), 0o644); err != nil {
		return "", err
	}
	return journal.HashDir(dir)
}

// stageRender 按时间点离屏导出 JPEG 帧序列。
func (st *pipeState) stageRender(dir string) (string, error) {
	var inputs []render.FrameInput
	idx := 0
	for ci := range st.cams {
		for f := 0; f < fixture.FramesPerCamera; f++ {
			inputs = append(inputs, render.FrameInput{
				Name:   fmt.Sprintf("cam%d_frame_%05d", ci, f),
				Source: st.payloads[idx],
				Seed:   fixture.FrameSeed(ci, f),
			})
			idx++
		}
	}
	frames, digest, err := render.RenderSequence(dir, inputs, 85)
	if err != nil {
		return "", err
	}
	st.frames = frames
	st.sha = digest
	list := ""
	for _, f := range frames {
		list += fmt.Sprintf("%s %s %dx%d\n", filepath.Base(f.Path), f.SHA256, f.Width, f.Height)
	}
	if err := os.WriteFile(filepath.Join(dir, "frames.sha256"), []byte(list), 0o644); err != nil {
		return "", err
	}
	return journal.HashDir(dir)
}
