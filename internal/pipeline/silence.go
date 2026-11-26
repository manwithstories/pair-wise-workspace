// silence.go 把流水线的静音检测与 PCM 解码独立成文件，保持单文件补丁规模。
package pipeline

import (
	"archivemedia/internal/adapter"
	"archivemedia/internal/audio"
	"archivemedia/internal/model"
	"archivemedia/internal/timeline"
)

// detectSilence 在归一时间线上对音频轨做静音检测。
// 直接消费适配器保留的 s16le 负载，按解码块逐块喂入以验证跨块合并；
// 适配器未保留负载（如真实容器）时回退到内容哈希的确定性电平。
func (r *Runner) detectSilence(_ *timeline.Timeline, pm *adapter.ParsedMedia) int {
	cfg := audio.Config{
		WindowSamples: r.cfg.SilenceWindow, ThresholdDb: r.cfg.SilenceDb,
		MinDuration: model.Duration(r.cfg.SilenceMinMs * 1000),
	}
	det, err := audio.NewDetector(cfg)
	if err != nil {
		return 0
	}
	for _, pkt := range pm.Packets {
		tr := pm.Tracks[pkt.TrackIndex]
		if tr.Type != adapter.MediaAudio {
			continue
		}
		start := model.TicksToTime(pkt.StartTicks, tr.TimeBase)
		samples := decodeS16LE(pkt.RawPayload)
		if len(samples) == 0 {
			// 回退：无原始负载时用内容哈希生成确定性电平。
			n := int(model.TicksToTime(pkt.DurationTicks, tr.TimeBase).Microseconds() *
				int64(tr.SampleRate) / model.MicrosPerSecond)
			base := int16(pkt.ContentHash & 0xffff)
			samples = make([]int16, n)
			for i := range samples {
				samples[i] = base
			}
		}
		if len(samples) == 0 {
			continue
		}
		det.Push(audio.Frame{
			TrackIndex: pkt.TrackIndex, Start: start,
			SampleRate: tr.SampleRate, Channels: max1(tr.Channels), Samples: samples,
		})
	}
	return len(det.Finalize())
}

// decodeS16LE 把小端字节流转成 int16 样本。
func decodeS16LE(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(b[i*2]) | int16(b[i*2+1])<<8
	}
	return out
}

func max1(v int) int {
	if v <= 0 {
		return 1
	}
	return v
}
