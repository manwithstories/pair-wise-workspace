// Package audio 负责 L16 音频的解码、重采样、声道归一与响度处理。
// 输入支持 8/16/48/96 kHz、单声道/立体声/5.1 的 16bit PCM（RFC 3551 L16），
// 经 go-astiav（libav*）统一输出 48kHz 双声道浮点，再做响度归一与真峰值限制。
package audio

import (
	"fmt"

	"github.com/asticode/go-astiav"
)

// 支持的输入规格。
var (
	supportedRates = map[int]bool{8000: true, 16000: true, 48000: true, 96000: true}
	supportedChans = map[int]bool{1: true, 2: true, 6: true}
)

// Target 交付规格：48kHz 双声道。
const (
	TargetRate    = 48000
	TargetChans   = 2
	bytesPerS16   = 2
)

// Decoder 把 L16 字节负载解码为 astiav 浮点帧。
// L16 是无损 PCM，无需编解码器，直接做 s16 → flt 的样本转换。
type Decoder struct {
	rate     int
	channels int
	layout   astiav.ChannelLayout
}

// NewDecoder 校验输入规格并构造解码器。不支持的参数返回 media 语义错误，
// 由引擎映射到退出码 3。
func NewDecoder(rate, channels int) (*Decoder, error) {
	if !supportedRates[rate] {
		return nil, &unsupported{reason: fmt.Sprintf("sample rate %d not in {8k,16k,48k,96k}", rate)}
	}
	if !supportedChans[channels] {
		return nil, &unsupported{reason: fmt.Sprintf("channel count %d not in {1,2,6}", channels)}
	}
	return &Decoder{rate: rate, channels: channels, layout: layoutFor(channels)}, nil
}

// Rate 返回输入采样率。
func (d *Decoder) Rate() int { return d.rate }

// Channels 返回输入声道数。
func (d *Decoder) Channels() int { return d.channels }

// Decode 把一个 RTP 负载（打包的 s16 交错样本）转为浮点 planar 帧。
// 负载字节数必须是 2*channels 的整数倍，否则返回坏负载故障。
func (d *Decoder) Decode(payload []byte) (*astiav.Frame, error) {
	frameBytes := bytesPerS16 * d.channels
	if len(payload) == 0 || len(payload)%frameBytes != 0 {
		return nil, &badPayload{reason: fmt.Sprintf(
			"L16 payload %d bytes not divisible by frame %d", len(payload), frameBytes)}
	}
	samples := len(payload) / frameBytes

	// 先建交错 s16 帧承载原始负载，再由 Normalizer 的 SWR 转 fltp。
	src := astiav.AllocFrame()
	if src == nil {
		return nil, fmt.Errorf("alloc source frame failed")
	}
	src.SetSampleFormat(astiav.SampleFormatS16)
	src.SetSampleRate(d.rate)
	src.SetChannelLayout(d.layout)
	src.SetNbSamples(samples)
	if err := src.AllocBuffer(0); err != nil {
		src.Free()
		return nil, fmt.Errorf("alloc source buffer: %w", err)
	}
	if err := src.Data().SetBytes(payload, 0); err != nil {
		src.Free()
		return nil, fmt.Errorf("load L16 payload: %w", err)
	}
	return src, nil
}

// layoutFor 把声道数映射为 FFmpeg 标准布局。
func layoutFor(channels int) astiav.ChannelLayout {
	switch channels {
	case 1:
		return astiav.ChannelLayoutMono
	case 6:
		return astiav.ChannelLayout5Point1
	default:
		return astiav.ChannelLayoutStereo
	}
}

// unsupported 与 badPayload 是包内错误类型，引擎按类别映射退出码。
type unsupported struct{ reason string }

func (e *unsupported) Error() string { return "unsupported: " + e.reason }

// IsUnsupported 报告错误是否为不支持的参数/编码。
func IsUnsupported(err error) bool {
	_, ok := err.(*unsupported)
	return ok
}

type badPayload struct{ reason string }

func (e *badPayload) Error() string { return "bad payload: " + e.reason }

// IsBadPayload 报告错误是否为坏负载。
func IsBadPayload(err error) bool {
	_, ok := err.(*badPayload)
	return ok
}
