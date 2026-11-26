// Package audio 在统一时间轴上做静音区间检测。
//
// 设计约束：
//   - 以采样窗计算 RMS 并换算 dBFS，静音判定阈值与最小时长可配置；
//   - 跨解码块的相邻静音自动合并，不产生碎片化区间；
//   - 声道缺帧（样本数与声道布局不符）会把受影响区间标记为不完整；
//   - 全部时间使用 model.MediaTime，与时间线/切分内核共用同一时基。
package audio

import (
	"math"

	"archivemedia/internal/model"
)

// Frame 是一块已解码的交织 PCM-s16 音频及其统一轴起点。
type Frame struct {
	TrackIndex int
	Start      model.MediaTime
	SampleRate int
	Channels   int
	// Samples 为交织有符号 16 位样本，长度应为 Channels*每声道样本数。
	Samples []int16
}

// Config 是静音检测参数。
type Config struct {
	WindowSamples int     // 单窗样本数（每声道），必须 >0
	ThresholdDb   float64 // dBFS，RMS 低于该值判为静音
	MinDuration   model.Duration
}

// Interval 是一个静音区间。
type Interval struct {
	TrackIndex int
	Start      model.MediaTime
	End        model.MediaTime
	Duration   model.Duration
	MinRmsDb   float64 // 区间内最低 RMS（dBFS）
	Complete   bool    // 声道缺帧或末窗不完整时为 false
}

// Detector 是有状态检测器，按块喂入并在 Finalize 输出合并后的区间。
type Detector struct {
	cfg        Config
	open       *Interval // 当前尚未闭合的静音区间
	results    []Interval
	lastEnd    model.MediaTime
	haveLast   bool
}

// NewDetector 构造检测器并校验参数。
func NewDetector(cfg Config) (*Detector, error) {
	if cfg.WindowSamples <= 0 {
		return nil, model.NewError(model.KindInvalidArgument, "audio: window samples must be positive")
	}
	if math.IsNaN(cfg.ThresholdDb) || math.IsInf(cfg.ThresholdDb, 0) {
		return nil, model.NewError(model.KindInvalidArgument, "audio: threshold must be finite dBFS")
	}
	if cfg.MinDuration < 0 {
		return nil, model.NewError(model.KindInvalidArgument, "audio: min duration must not be negative")
	}
	return &Detector{cfg: cfg}, nil
}

const fullScale = 32768.0 // int16 满量程

// Push 喂入一个解码块。
func (d *Detector) Push(f Frame) error {
	if f.SampleRate <= 0 || f.Channels <= 0 {
		return model.NewError(model.KindInvalidArgument,
			"audio: frame %d has invalid rate/channels", f.TrackIndex)
	}
	complete := true
	perChannel := len(f.Samples) / f.Channels
	if len(f.Samples)%f.Channels != 0 {
		// 声道缺帧：尾部样本无法构成完整帧，受影响区间标记不完整。
		complete = false
	}
	usPerSample := model.MicrosPerSecond / int64(f.SampleRate)
	win := d.cfg.WindowSamples
	for off := 0; off < perChannel; off += win {
		n := win
		winComplete := complete
		if off+n > perChannel {
			n = perChannel - off
			winComplete = false // 末窗样本不足
		}
		rmsDb := rmsDb(f.Samples, f.Channels, off, n)
		winStart := f.Start.Add(model.MediaTime(int64(off) * usPerSample))
		winEnd := f.Start.Add(model.MediaTime(int64(off+n) * usPerSample))
		silent := rmsDb < d.cfg.ThresholdDb
		switch {
		case silent && d.open != nil:
			// 延续当前区间（即便中间经过块边界，也在此合并）。
			d.open.End = winEnd
			d.open.Duration = model.DurationOf(d.open.Start, winEnd)
			if rmsDb < d.open.MinRmsDb {
				d.open.MinRmsDb = rmsDb
			}
			if !winComplete {
				d.open.Complete = false
			}
		case silent:
			d.open = &Interval{
				TrackIndex: f.TrackIndex, Start: winStart, End: winEnd,
				Duration: model.DurationOf(winStart, winEnd),
				MinRmsDb: rmsDb, Complete: winComplete,
			}
		case !silent && d.open != nil:
			d.closeOpen()
		}
	}
	d.lastEnd = f.Start.Add(model.MediaTime(int64(perChannel) * usPerSample))
	d.haveLast = true
	return nil
}

func (d *Detector) closeOpen() {
	if d.open == nil {
		return
	}
	if d.open.Duration >= d.cfg.MinDuration {
		d.results = append(d.results, *d.open)
	}
	d.open = nil
}

// Finalize 闭合末区间并返回合并、过滤后的静音区间。
func (d *Detector) Finalize() []Interval {
	d.closeOpen()
	out := d.results
	d.results = nil
	return out
}

// rmsDb 计算从每声道偏移 off 起、n 个每声道样本窗口的 RMS（dBFS）。
// 交织布局下每声道第 k 个样本位于 k*Channels+ch，全部声道一并纳入能量。
func rmsDb(samples []int16, channels, off, n int) float64 {
	if n <= 0 {
		return math.Inf(-1)
	}
	var sumSq float64
	var count float64
	for k := off; k < off+n; k++ {
		for ch := 0; ch < channels; ch++ {
			idx := k*channels + ch
			if idx >= len(samples) {
				continue
			}
			v := float64(samples[idx])
			sumSq += v * v
			count++
		}
	}
	if count == 0 {
		return math.Inf(-1)
	}
	rms := math.Sqrt(sumSq / count)
	if rms == 0 {
		return math.Inf(-1) // 纯静音
	}
	return 20 * math.Log10(rms / fullScale)
}

// Detect 是便捷入口：一次性处理多块并返回结果。
func Detect(frames []Frame, cfg Config) ([]Interval, error) {
	d, err := NewDetector(cfg)
	if err != nil {
		return nil, err
	}
	for i := range frames {
		if err := d.Push(frames[i]); err != nil {
			return nil, err
		}
	}
	return d.Finalize(), nil
}
