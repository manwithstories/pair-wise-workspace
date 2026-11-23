// Package silence 基于滑动窗口计算 PCM 峰值与均方根（RMS），
// 识别静音区间并按可配置规则跨声道聚合。
package silence

import (
	"math"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/fault"
	"media-pipeline/internal/media"
)

// ChannelRule 决定多声道如何聚合为每帧的响度判定。
type ChannelRule uint8

const (
	// RuleMax 任一声道超过阈值即视为非静音（最严格）。
	RuleMax ChannelRule = iota
	// RuleMean 所有声道平均后再与阈值比较。
	RuleMean
)

// Config 配置静音检测。阈值为满量程的比例（0..1，定点放大 1e6）。
type Config struct {
	WindowFrames   uint64        // 滑动窗口帧数
	MinSilenceFrames uint64      // 最短静音持续，短于此不得切段
	PeakThreshold  uint32        // 峰值阈值（满量程比例 *1e6）
	RMSThreshold   uint32        // RMS 阈值（满量程比例 *1e6）
	Channels       uint16
	BitsPerSample  uint16
	Rule           ChannelRule
}

// Interval 是一个确认的静音区间，附带峰值与 RMS（比例 *1e6）。
type Interval struct {
	FirstSample uint64
	EndSample   uint64
	Frames      uint64
	Peak        uint32
	RMS         uint32
}

// scale 把阈值比例放大为整数，全程避免浮点带来的不确定性。
const scale = 1_000_000

// Detector 跨块维护滑动窗口，保证窗口跨块时统计结果一致。
type Detector struct {
	cfg    Config
	tb     clock.Timebase
	origin uint64 // 已处理到的全局采样序号

	// 环形窗口：保存最近 WindowFrames 帧的峰值与平方和（满量程比例定点）。
	ringPeak []uint32
	ringSq   []uint64
	ringPos  int
	ringFill int
	sumSq    uint64

	// 静音游程状态（绝对采样序号）及其区间级统计（仅含游程内的帧）。
	silenceStart  uint64
	silencePeak   uint32
	silenceSqSum  uint64
	silenceFrames uint64
	inSilence     bool
}

// New 构造检测器。
func New(cfg Config, tb clock.Timebase) (*Detector, error) {
	if err := tb.Valid(); err != nil {
		return nil, fault.New(fault.KindOther, "", -1, -1, false,
			"invalid timebase").WithOp("silence.new").Wrap(err)
	}
	if cfg.WindowFrames == 0 {
		return nil, fault.New(fault.KindOther, "", -1, -1, false,
			"window frames must be > 0").WithOp("silence.new")
	}
	return &Detector{
		cfg: cfg, tb: tb,
		ringPeak: make([]uint32, cfg.WindowFrames),
		ringSq:   make([]uint64, cfg.WindowFrames),
	}, nil
}

// frameLevel 按配置规则把一帧的所有声道聚合为峰值与平方（比例定点）。
func (d *Detector) frameLevel(samples []int32) (peak uint32, sq uint64) {
	var sumPeak, sumSq uint64
	for _, v := range samples {
		amp := abs32(v)
		norm := uint64(amp) * scale / uint64(fullScale(d.cfg.BitsPerSample))
		if norm > uint64(math.MaxUint32) {
			norm = math.MaxUint32
		}
		sumPeak += norm
		sumSq += norm * norm
		if uint32(norm) > peak {
			peak = uint32(norm)
		}
	}
	switch d.cfg.Rule {
	case RuleMean:
		n := uint64(len(samples))
		return uint32(sumPeak / n), sumSq / n
	default: // RuleMax
		return peak, sumSq
	}
}

func fullScale(bits uint16) int32 {
	// 整数 PCM 的正满量程：8bit 为 127，16bit 为 32767，依此类推。
	return int32(1)<<(bits-1) - 1
}

func abs32(v int32) int32 {
	if v < 0 {
		// 避开 -2^31 取负溢出。
		if v == -1<<31 {
			return 1<<31 - 1
		}
		return -v
	}
	return v
}

// Push 处理一个 PCM 块，返回新确认的静音区间。
// 窗口跨块时由环形缓冲承接，统计与整段输入完全一致。
func (d *Detector) Push(b media.Block) ([]Interval, error) {
	frames, err := b.FrameCount()
	if err != nil {
		return nil, err
	}
	var out []Interval
	fs := d.tb.FrameSize()
	for f := uint64(0); f < frames; f++ {
		raw := b.PCM[f*uint64(fs) : (f+1)*uint64(fs)]
		samples := decodeFrame(raw, d.cfg.BitsPerSample, d.cfg.Channels)
		peak, sq := d.frameLevel(samples)
		d.slide(peak, sq)

		winPeak, winRMS := d.windowStats()
		silent := winPeak <= d.cfg.PeakThreshold && winRMS <= d.cfg.RMSThreshold
		at := d.origin + f
		if silent && !d.inSilence {
			d.inSilence, d.silenceStart = true, at
			d.silencePeak, d.silenceSqSum, d.silenceFrames = 0, 0, 0
		}
		if silent {
			// 区间统计只累计静音游程内的帧，不混入滑窗历史响帧。
			if peak > d.silencePeak {
				d.silencePeak = peak
			}
			d.silenceSqSum += sq
			d.silenceFrames++
		}
		if !silent && d.inSilence {
			if iv, ok := d.close(at); ok {
				out = append(out, iv)
			}
			d.inSilence = false
		}
	}
	d.origin += frames
	return out, nil
}

// Flush 结束流，若尾部仍处于静音则尝试闭合最后一个区间。
func (d *Detector) Flush() []Interval {
	if !d.inSilence {
		return nil
	}
	iv, ok := d.close(d.origin)
	d.inSilence = false
	if !ok {
		return nil
	}
	return []Interval{iv}
}

func (d *Detector) close(end uint64) (Interval, bool) {
	frames := end - d.silenceStart
	if frames < d.cfg.MinSilenceFrames {
		return Interval{}, false // 短于阈值的静音不得切段
	}
	var rms uint32
	if d.silenceFrames > 0 {
		rms = uint32(isqrt(d.silenceSqSum / d.silenceFrames))
	}
	return Interval{
		FirstSample: d.silenceStart,
		EndSample:   end,
		Frames:      frames,
		Peak:        d.silencePeak,
		RMS:         rms,
	}, true
}

// slide 把新帧压入环形窗口并维护平方和与峰值聚合。
func (d *Detector) slide(peak uint32, sq uint64) {
	old := d.ringPos
	if d.ringFill == len(d.ringPeak) {
		d.sumSq -= d.ringSq[old]
	} else {
		d.ringFill++
	}
	d.ringSq[old] = sq
	d.ringPeak[old] = peak
	d.ringPos = (d.ringPos + 1) % len(d.ringPeak)
	d.sumSq += sq
}

func (d *Detector) windowStats() (peak uint32, rms uint32) {
	for i := 0; i < d.ringFill; i++ {
		if p := d.ringPeak[i]; p > peak {
			peak = p
		}
	}
	mean := d.sumSq / uint64(d.ringFill)
	return peak, uint32(isqrt(mean))
}

// isqrt 计算整数平方根，避免浮点不确定性。
func isqrt(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	x := n
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}
