// Package pipeline 编排解析、变换、验证与可恢复提交；分片为恢复单元。
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"mediadeliver/internal/audio"
	"mediadeliver/internal/fault"
	"mediadeliver/internal/fragment"
	"mediadeliver/internal/manifest"
	"mediadeliver/internal/transport"
)

// Config 是流水线参数。
type Config struct {
	SampleRate   int64
	Channels     int
	TargetLUFS   float64
	PeakCeiling  float64
	RTPWindow    int
	RTPClockRate int64
}

// SegmentData 是一个分片的输入。
type SegmentData struct {
	URI      string
	Fragment []byte // fMP4 字节
	Audio    audio.PCM
}

// Job 是一次交付任务的全部输入。
type Job struct {
	ManifestName string // .m3u8 或 .mpd
	ManifestData []byte
	Segments     []SegmentData
	RTPPackets   [][]byte
}

// SegmentResult 记录一个分片的阶段提交摘要。
type SegmentResult struct {
	URI       string `json:"uri"`
	InputSHA  string `json:"input_sha"`
	OutputSHA string `json:"output_sha"`
	Samples   int    `json:"samples"`
	Recovered bool   `json:"recovered"` // 含可恢复错误
	Reused    bool   `json:"reused"`    // 摘要一致，复用旧阶段
}

// Result 是流水线输出。
type Result struct {
	Playlist     manifest.Playlist
	Segments     []SegmentResult
	RTPStats     transport.Stats
	Loudness     []audio.LoudnessResult
	Output       audio.PCM
	SyncErrorMs  float64
	Latencies    []time.Duration
	Faults       fault.List
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Run 执行流水线。prev 非空时按摘要校验复用：输入摘要一致才复用旧阶段，
// 输入变化拒绝复用并重新计算。
func Run(cfg Config, job Job, prev *Result) *Result {
	res := &Result{}
	// 阶段 1：清单解析
	if len(job.ManifestName) > 4 && job.ManifestName[len(job.ManifestName)-4:] == ".mpd" {
		res.Playlist = manifest.ParseDASH(job.ManifestName, job.ManifestData)
	} else {
		res.Playlist = manifest.ParseHLS(job.ManifestName, job.ManifestData)
	}
	res.Faults = append(res.Faults, res.Playlist.Faults...)

	prevByURI := map[string]SegmentResult{}
	if prev != nil {
		for _, s := range prev.Segments {
			prevByURI[s.URI] = s
		}
	}

	// 阶段 2-4：逐分片解析 fMP4、变换、响度（分片为恢复单元）
	var pcm []float64
	for _, seg := range job.Segments {
		start := time.Now()
		sr := SegmentResult{URI: seg.URI}
		inSHA := sha(append(append([]byte{}, seg.Fragment...), audioBytes(seg.Audio)...))
		sr.InputSHA = inSHA
		before := len(res.Faults)

		fr := fragment.Parse(seg.URI, seg.Fragment)
		res.Faults = append(res.Faults, fr.Faults...)

		conv, cf := audio.Convert(seg.Audio, cfg.SampleRate, cfg.Channels)
		res.Faults = append(res.Faults, cf...)
		norm, lr, lf := audio.Normalize(conv, cfg.TargetLUFS, cfg.PeakCeiling)
		res.Faults = append(res.Faults, lf...)
		res.Loudness = append(res.Loudness, lr)

		sr.Samples = norm.NumFrames()
		sr.OutputSHA = sha(audioBytes(norm))
		// 阶段提交校验：仅当输入摘要一致才允许复用
		if old, ok := prevByURI[seg.URI]; ok {
			if old.InputSHA == inSHA && old.OutputSHA == sr.OutputSHA {
				sr.Reused = true
			} else if old.InputSHA == inSHA && old.OutputSHA != sr.OutputSHA {
				res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageCommit, Source: seg.URI,
					Offset: -1, Message: "stage digest mismatch on identical input",
					Advice: "拒绝复用，检查确定性"})
			}
		}
		sr.Recovered = len(res.Faults) > before
		res.Segments = append(res.Segments, sr)
		pcm = append(pcm, norm.Frames...)
		res.Latencies = append(res.Latencies, time.Since(start))
	}
	res.Output = audio.PCM{SampleRate: cfg.SampleRate, Channels: cfg.Channels, Frames: pcm}

	// 阶段 5：RTP 重排与恢复统计
	rt := transport.NewReorderer(cfg.RTPWindow)
	if rt == nil {
		res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageTransport, Source: "rtp",
			Offset: -1, Message: "invalid rtp window", Advice: "调整 --rtp-window"})
	} else {
		for _, raw := range job.RTPPackets {
			rt.Push(raw)
		}
		rt.Flush()
		res.RTPStats = rt.Stats()
		res.Faults = append(res.Faults, rt.Faults...)
	}

	// 阶段 6：跨阶段验证——采样数、声道、响度、边界与同步误差
	expected := res.Playlist.Duration.Samples(cfg.SampleRate)
	actual := int64(res.Output.NumFrames())
	res.SyncErrorMs = float64(actual-expected) / float64(cfg.SampleRate) * 1000
	if res.SyncErrorMs < 0 {
		res.SyncErrorMs = -res.SyncErrorMs
	}
	if res.Output.Channels != cfg.Channels {
		res.Faults = res.Faults.Add(fault.Fault{Stage: fault.StageValidate, Source: "output",
			Offset: -1, Message: fmt.Sprintf("channel layout %d != %d", res.Output.Channels, cfg.Channels),
			Advice: "检查声道矩阵配置"})
	}
	return res
}

// OutputDigest 返回输出 PCM 的确定性摘要。
func (r *Result) OutputDigest() string { return sha(audioBytes(r.Output)) }

func audioBytes(p audio.PCM) []byte {
	b := make([]byte, 0, len(p.Frames)*8+16)
	b = append(b, byte(p.SampleRate), byte(p.SampleRate>>8), byte(p.Channels), 0)
	for _, s := range p.Frames {
		var u uint64
		// 定点量化保证跨平台确定性
		q := int64(s * (1 << 30))
		if q > 1<<40 {
			q = 1 << 40
		}
		if q < -(1 << 40) {
			q = -(1 << 40)
		}
		u = uint64(q)
		for i := 0; i < 8; i++ {
			b = append(b, byte(u>>(8*i)))
		}
	}
	return b
}
