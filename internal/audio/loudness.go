package audio

import (
	"math"

	"mediadeliver/internal/fault"
)

// 响度归一化的确定性参数。
const (
	MinDurationSec = 0.25  // 低于该时长判定为 too_short
	MaxGainDB      = 40.0  // 增益上限，超限记 gain_limited
	silenceFloor   = 1e-10 // 均方低于该值判定静音
)

// LoudnessResult 是响度调整的确定性结果。
type LoudnessResult struct {
	Status     string  `json:"status"` // ok | silence | too_short | gain_limited
	InputLUFS  float64 `json:"input_lufs"`
	OutputLUFS float64 `json:"output_lufs"`
	GainDB     float64 `json:"gain_db"`
	PeakDBTP   float64 `json:"peak_dbtp"`
}

// MeasureLUFS 以 RMS 近似计算响度（K 加权简化为固定偏移）。
func MeasureLUFS(p PCM) float64 {
	if len(p.Frames) == 0 {
		return -math.MaxFloat64
	}
	var ms float64
	for _, s := range p.Frames {
		ms += s * s
	}
	ms /= float64(len(p.Frames))
	if ms < silenceFloor {
		return -math.MaxFloat64
	}
	return 10*math.Log10(ms) - 0.691
}

// PeakDB 返回峰值电平（dBTP 近似）。
func PeakDB(p PCM) float64 {
	peak := 0.0
	for _, s := range p.Frames {
		if a := math.Abs(s); a > peak {
			peak = a
		}
	}
	if peak < 1e-12 {
		return -math.MaxFloat64
	}
	return 20 * math.Log10(peak)
}

// Normalize 按目标响度与峰值上限调整；静音、过短与超限增益产生确定性状态。
func Normalize(p PCM, targetLUFS, peakCeilingDBTP float64) (PCM, LoudnessResult, fault.List) {
	res := LoudnessResult{Status: "ok", InputLUFS: MeasureLUFS(p), PeakDBTP: PeakDB(p)}
	var faults fault.List
	dur := float64(p.NumFrames()) / float64(p.SampleRate)
	switch {
	case res.InputLUFS == -math.MaxFloat64:
		res.Status = "silence"
		res.OutputLUFS = res.InputLUFS
		faults = faults.Add(fault.Fault{Stage: fault.StageLoudness, Source: "pcm",
			Offset: -1, Recoverable: true, Message: "silent input, gain skipped",
			Advice: "确认采集链路是否静音"})
		return p, res, faults
	case dur < MinDurationSec:
		res.Status = "too_short"
		res.OutputLUFS = res.InputLUFS
		faults = faults.Add(fault.Fault{Stage: fault.StageLoudness, Source: "pcm",
			Offset: -1, Recoverable: true, Message: "audio too short for loudness",
			Advice: "保留原始电平输出"})
		return p, res, faults
	}
	gain := targetLUFS - res.InputLUFS
	// 峰值余量约束
	if headroom := peakCeilingDBTP - res.PeakDBTP; gain > headroom {
		gain = headroom
		res.Status = "gain_limited"
	}
	if math.Abs(gain) > MaxGainDB {
		gain = math.Copysign(MaxGainDB, gain)
		res.Status = "gain_limited"
	}
	if res.Status == "gain_limited" {
		faults = faults.Add(fault.Fault{Stage: fault.StageLoudness, Source: "pcm",
			Offset: -1, Recoverable: true, Message: "gain clamped by ceiling/limit",
			Advice: "检查源电平或调整目标参数"})
	}
	res.GainDB = gain
	lin := math.Pow(10, gain/20)
	out := PCM{SampleRate: p.SampleRate, Channels: p.Channels, Frames: make([]float64, len(p.Frames))}
	for i, s := range p.Frames {
		out.Frames[i] = s * lin
	}
	res.OutputLUFS = MeasureLUFS(out)
	res.PeakDBTP = PeakDB(out)
	return out, res, faults
}
