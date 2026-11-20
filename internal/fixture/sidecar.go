package fixture

import (
	"encoding/json"
	"os"

	"github.com/pion/rtp"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
	"mediarepair/internal/parse"
)

// sidecarJSON 是生成的时间线 sidecar 结构，字段与 parse.timelineJSON 保持一致。
type sidecarJSON struct {
	Version string `json:"version"`
	Tracks  []struct {
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
		FirstSample  uint64 `json:"first_sample"`
		SampleCount  uint64 `json:"sample_count"`
	} `json:"clips"`
	Transitions []struct {
		ID           string `json:"id"`
		TrackID      uint32 `json:"track_id"`
		FromClipID   string `json:"from_clip_id"`
		ToClipID     string `json:"to_clip_id"`
		StartTick    uint64 `json:"start_tick"`
		DurationTick uint64 `json:"duration_tick"`
		FirstSample  uint64 `json:"first_sample"`
		SampleCount  uint64 `json:"sample_count"`
	} `json:"transitions"`
	RTPTrackID uint32 `json:"rtp_track_id"`
}

// writeSidecar 把时间线写成确定性 JSON（键顺序由结构体字段顺序固定）。
func writeSidecar(path string, tl *model.Timeline) error {
	var sc sidecarJSON
	sc.Version = parse.TimelineVersion
	sc.RTPTrackID = tl.RTPTrackID
	for _, t := range tl.Tracks {
		sc.Tracks = append(sc.Tracks, struct {
			ID        uint32 `json:"id"`
			Kind      string `json:"kind"`
			Timescale uint32 `json:"timescale"`
			Name      string `json:"name"`
		}{t.ID, string(t.Kind), t.Timescale, t.Name})
	}
	for _, c := range tl.Clips {
		sc.Clips = append(sc.Clips, struct {
			ID           string `json:"id"`
			TrackID      uint32 `json:"track_id"`
			Index        int    `json:"index"`
			StartTick    uint64 `json:"start_tick"`
			DurationTick uint64 `json:"duration_tick"`
			FirstSample  uint64 `json:"first_sample"`
			SampleCount  uint64 `json:"sample_count"`
		}{c.ID, c.TrackID, c.Index, c.StartTick, c.DurationTick, c.FirstSample, c.SampleCount})
	}
	for _, tr := range tl.Transitions {
		sc.Transitions = append(sc.Transitions, struct {
			ID           string `json:"id"`
			TrackID      uint32 `json:"track_id"`
			FromClipID   string `json:"from_clip_id"`
			ToClipID     string `json:"to_clip_id"`
			StartTick    uint64 `json:"start_tick"`
			DurationTick uint64 `json:"duration_tick"`
			FirstSample  uint64 `json:"first_sample"`
			SampleCount  uint64 `json:"sample_count"`
		}{tr.ID, tr.TrackID, tr.FromClipID, tr.ToClipID, tr.StartTick, tr.DurationTick, tr.FirstSample, tr.SampleCount})
	}
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "编码时间线失败", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "写时间线失败", err)
	}
	return nil
}

// RTPBaseTimestamp 是夹具 RTP 流的起始时间戳，取接近 32 位域上沿的值，
// 便于在 clean 之外的模式下于有限样本内触发回绕。
const RTPBaseTimestamp uint32 = 0xFFFFF000

// rtpTimestamps 生成 RTP 时间戳序列，按模式注入回绕、重复或倒退。
func rtpTimestamps(spec Spec) []uint32 {
	n := spec.VideoSamples
	out := make([]uint32, n)
	ts := RTPBaseTimestamp
	for i := range out {
		out[i] = ts
		ts += videoBaseDelta
	}
	switch spec.Pattern {
	case PatternWrap:
		// 默认起点已接近上沿，自然回绕；无需额外注入。
	case PatternDuplicate:
		if n > 4 {
			out[n/2] = out[n/2-1]
		}
	case PatternRegression:
		if n > 6 {
			out[n/2] = out[n/2-2] - videoBaseDelta
		}
	}
	return out
}

// writeRTPDump 写 RTP dump：每条记录为 [2 字节大端长度][RTP 包]。
// 仅写入元数据包头，不含真实编码负载，也不发送任何网络数据。
func writeRTPDump(path string, spec Spec) error {
	f, err := os.Create(path)
	if err != nil {
		return fault.Wrap(fault.StageIO, 0, -1, false, "创建 RTP dump 失败", err)
	}
	defer f.Close()
	stamps := rtpTimestamps(spec)
	buf := make([]byte, 12+12)
	for i, ts := range stamps {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: ts, SSRC: 0xA1B2C3D4},
			Payload: make([]byte, 12),
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return fault.Wrap(fault.StageIO, 0, -1, false, "编码 RTP 包失败", err)
		}
		buf = buf[:0]
		buf = append(buf, byte(len(raw)>>8), byte(len(raw)))
		buf = append(buf, raw...)
		if _, err := f.Write(buf); err != nil {
			return fault.Wrap(fault.StageIO, 0, -1, false, "写 RTP dump 失败", err)
		}
	}
	return nil
}
