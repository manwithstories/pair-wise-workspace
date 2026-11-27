package pipeline

import (
	"context"

	"bcastqc/internal/ebml"
	"bcastqc/internal/jitter"
	"bcastqc/internal/media"
	"bcastqc/internal/mux"
	rtpx "bcastqc/internal/rtp"
	avsync "bcastqc/internal/sync"
)

// frameMeta 记录原始帧的时间与关键帧标记，供重组后还原块。
type frameMeta struct {
	timeNS int64
	key    bool
}

// reassembledFrame 是剥掉 RTP 头、按 Marker 拼回的一帧。
type reassembledFrame struct {
	track   uint32
	timeNS  int64
	key     bool
	payload []byte
}

// recoverAndReassemble 驱动两条轨道的最终恢复，把报文重组回帧，
// 视频遇未恢复缺口时跳到下一关键帧；音频缺口不补数据。
func recoverAndReassemble(
	ctx context.Context,
	rcvVideo, rcvAudio *jitter.Receiver,
	doc *ebml.Document,
	aligner *avsync.Aligner,
	out *[]mux.MuxBlock,
	m *Metrics,
) error {
	videoMeta, audioMeta := indexFrames(doc)

	videoPackets, videoGaps := rcvVideo.Output()
	audioPackets, audioGaps := rcvAudio.Output()

	videoFrames := reassembleTrack(media.TrackVideo, videoPackets, videoGaps, videoMeta)
	audioFrames := reassembleTrack(media.TrackAudio, audioPackets, audioGaps, audioMeta)

	// 交错回放，保持输出时间线单调。
	vi, ai := 0, 0
	for vi < len(videoFrames) || ai < len(audioFrames) {
		if ai < len(audioFrames) && (vi >= len(videoFrames) ||
			audioFrames[ai].timeNS <= videoFrames[vi].timeNS) {
			fr := audioFrames[ai]
			*out = append(*out, mux.MuxBlock{Track: fr.track, TimeNS: fr.timeNS, Key: fr.key, Payload: fr.payload})
			_ = aligner.Add(avsync.TimedPoint{Track: media.TrackAudio, TimeNS: fr.timeNS})
			ai++
		} else if vi < len(videoFrames) {
			fr := videoFrames[vi]
			*out = append(*out, mux.MuxBlock{Track: fr.track, TimeNS: fr.timeNS, Key: fr.key, Payload: fr.payload})
			_ = aligner.Add(avsync.TimedPoint{Track: media.TrackVideo, TimeNS: fr.timeNS, Key: fr.key})
			vi++
		}
	}
	return nil
}

// indexFrames 按时间戳建立轨道帧元数据索引。
func indexFrames(doc *ebml.Document) (video, audio map[int64]frameMeta) {
	video = make(map[int64]frameMeta)
	audio = make(map[int64]frameMeta)
	for _, b := range doc.Blocks {
		meta := frameMeta{timeNS: b.TimeNS, key: b.Keyframe}
		if b.Track == media.TrackVideo {
			video[b.TimeNS] = meta
		} else {
			audio[b.TimeNS] = meta
		}
	}
	return video, audio
}

// reassembleTrack 用 pion 解包剥离 RTP 头，按 Marker 拼回帧，
// 并依据未恢复缺口决定丢弃：视频跳到下一关键帧，音频仅跳过缺失帧。
func reassembleTrack(track uint32, packets []rtpx.Packet, gaps []jitter.Gap,
	meta map[int64]frameMeta) []reassembledFrame {
	dec := rtpx.NewDecoder(track)
	type frag struct {
		timeNS int64
		key    bool
		data   []byte
		marker bool
	}
	var frags []frag
	for _, p := range packets {
		d, err := dec.Decode(p.Raw)
		if err != nil {
			continue // 损坏报文跳过，不污染帧
		}
		frags = append(frags, frag{
			timeNS: p.TimeNS, key: p.Key,
			data: append([]byte(nil), d.Payload...), marker: d.Marker,
		})
	}

	// 视频存在永久缺口时，进入“跳到下一关键帧”模式，期间帧全部丢弃。
	skipUntilKey := track == media.TrackVideo && len(gaps) > 0
	var frames []reassembledFrame
	var cur reassembledFrame
	have := false

	flush := func() {
		if !have {
			return
		}
		if fm, ok := meta[cur.timeNS]; ok {
			cur.key = fm.key
		}
		if !skipUntilKey {
			frames = append(frames, cur)
		} else if fm, ok := meta[cur.timeNS]; ok && fm.key {
			skipUntilKey = false
			frames = append(frames, cur) // 关键帧本身可安全起播
		}
		cur = reassembledFrame{}
		have = false
	}

	for _, f := range frags {
		if have && f.timeNS != cur.timeNS {
			flush()
		}
		if !have {
			cur = reassembledFrame{track: track, timeNS: f.timeNS, key: f.key}
			have = true
		}
		cur.payload = append(cur.payload, f.data...)
		if f.marker {
			flush()
		}
	}
	flush()
	return frames
}

// fillTransportMetrics 汇总两条轨道的传输与缓存指标。
func fillTransportMetrics(m *Metrics, rcvVideo, rcvAudio *jitter.Receiver) {
	vm := rcvVideo.Finalize()
	am := rcvAudio.Finalize()
	m.Lost = vm.Lost + am.Lost
	m.Recovered = vm.Recovered + am.Recovered
	if m.Lost > 0 {
		m.RecoveryPct = float64(m.Recovered) / float64(m.Lost) * 100
	} else {
		m.RecoveryPct = 100
	}
	p95 := vm.LatencyP95NS
	if am.LatencyP95NS > p95 {
		p95 = am.LatencyP95NS
	}
	m.LatencyP95MS = p95 / 1_000_000
	peak := vm.PeakBufferBytes
	if am.PeakBufferBytes > peak {
		peak = am.PeakBufferBytes
	}
	m.PeakBufferBytes = peak
}
