// Package audio 实现归档音频的统一化处理：重采样到 48 kHz、声道归一为立体声、
// 响度归一与真峰值限制。全部处理在确定的浮点 PCM 域进行，可重复复现。
package audio

import (
	"math"

	"github.com/asticode/go-astiav"

	"archivemedia/internal/media"
	"archivemedia/internal/packet"
)

// TargetRate 是归档目标采样率。
const TargetRate = 48_000

// Buffer 是统一化后的交错 float32 PCM 缓冲（立体声）。
type Buffer struct {
	// Samples 为交错立体声 float32，长度 = 帧数*2。
	Samples []float32
	// Frames 为每声道采样点数。
	Frames int
}

// Resampler 把任意输入格式的 PCM 帧转换为 48 kHz 立体声平面浮点，
// 再交错输出。内部 swr 按首帧自动配置（FFmpeg 9 行为）。
// 输入帧统一使用 planar FLTP，按 AVFrame linesize 对齐填充，避免紧密缓冲被拒。
type Resampler struct {
	swr *astiav.SoftwareResampleContext
	// 已配置的输入签名，输入格式变化时需要重建 swr。
	inRate   int
	inCh     int
	outFrame *astiav.Frame
}

// NewResampler 构造重采样器。
func NewResampler() *Resampler {
	out := astiav.AllocFrame()
	out.SetChannelLayout(astiav.ChannelLayoutStereo)
	out.SetSampleFormat(astiav.SampleFormatFltp)
	out.SetSampleRate(TargetRate)
	return &Resampler{
		swr:      astiav.AllocSoftwareResampleContext(),
		outFrame: out,
	}
}

// Close 释放 C 资源。
func (r *Resampler) Close() {
	if r.outFrame != nil {
		r.outFrame.Free()
		r.outFrame = nil
	}
	if r.swr != nil {
		r.swr.Free()
		r.swr = nil
	}
}

// ensureConfig 在输入签名首次出现或变化时记录；swr 由 ConvertFrame 按帧自配置。
func (r *Resampler) ensureConfig(f *packet.PCMFrame) error {
	if r.inRate == f.SampleRate && r.inCh == f.Channels {
		return nil
	}
	// 输入变化：重建 swr 以避免残留延迟污染新流。
	if r.inRate != 0 {
		r.swr.Free()
		r.swr = astiav.AllocSoftwareResampleContext()
	}
	r.inRate = f.SampleRate
	r.inCh = f.Channels
	return nil
}

// Convert 转换一帧交错 float32 PCM 为 48k 立体声交错 PCM。
// 输入帧采样率/声道数可与前帧不同（可变时长、混合来源）。
func (r *Resampler) Convert(f *packet.PCMFrame) (*Buffer, error) {
	if f == nil || f.NbSamples <= 0 {
		return &Buffer{}, nil
	}
	if err := r.ensureConfig(f); err != nil {
		return nil, err
	}
	// 输入帧每帧分配（astiav 标准安全用法，避免跨包缓冲残留导致不确定性）。
	src := astiav.AllocFrame()
	defer src.Free()
	src.SetNbSamples(f.NbSamples)
	src.SetSampleFormat(astiav.SampleFormatFltp)
	src.SetSampleRate(f.SampleRate)
	src.SetChannelLayout(layoutFor(f.Channels))
	if err := src.AllocBuffer(0); err != nil {
		return nil, media.E(media.KindAudio, "audio.resample",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"输入帧缓冲分配失败", err)
	}
	// 按帧实际 linesize 把交错样本解交织写入平面缓冲，保证对齐正确。
	if err := writePlanar(src, f); err != nil {
		return nil, err
	}
	// 输出帧容量按最保守上界预留，每帧 Unref 后重分配（astiav 标准模式）。
	est := f.NbSamples*TargetRate/f.SampleRate + 32
	r.outFrame.Unref()
	r.outFrame.SetChannelLayout(astiav.ChannelLayoutStereo)
	r.outFrame.SetSampleFormat(astiav.SampleFormatFltp)
	r.outFrame.SetSampleRate(TargetRate)
	r.outFrame.SetNbSamples(est)
	if err := r.outFrame.AllocBuffer(0); err != nil {
		return nil, media.E(media.KindAudio, "audio.resample",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"输出帧缓冲分配失败", err)
	}
	if err := r.swr.ConvertFrame(src, r.outFrame); err != nil {
		return nil, media.E(media.KindAudio, "audio.resample",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"重采样转换失败", err)
	}
	return r.readOut()
}

// Flush 冲刷重采样器内部延迟，返回残余样本。
func (r *Resampler) Flush() (*Buffer, error) {
	if r.inRate == 0 {
		return &Buffer{}, nil
	}
	est := TargetRate / 10
	r.outFrame.Unref()
	r.outFrame.SetChannelLayout(astiav.ChannelLayoutStereo)
	r.outFrame.SetSampleFormat(astiav.SampleFormatFltp)
	r.outFrame.SetSampleRate(TargetRate)
	r.outFrame.SetNbSamples(est)
	if err := r.outFrame.AllocBuffer(0); err != nil {
		return nil, media.E(media.KindAudio, "audio.flush",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"冲刷缓冲分配失败", err)
	}
	if err := r.swr.ConvertFrame(nil, r.outFrame); err != nil {
		return nil, media.E(media.KindAudio, "audio.flush",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"冲刷重采样器失败", err)
	}
	return r.readOut()
}

// readOut 把平面立体声输出交错化为 Buffer。
func (r *Resampler) readOut() (*Buffer, error) {
	n := r.outFrame.NbSamples()
	if n <= 0 {
		return &Buffer{}, nil
	}
	raw, err := r.outFrame.Data().Bytes(1)
	if err != nil {
		return nil, media.E(media.KindAudio, "audio.read-out",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"读取重采样输出失败", err)
	}
	planar := bytesToFloat32(raw)
	if len(planar) < n*2 {
		return nil, media.E(media.KindAudio, "audio.read-out",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"重采样输出样本不足", nil)
	}
	out := make([]float32, n*2)
	for i := 0; i < n; i++ {
		out[i*2] = softClip(planar[i])
		out[i*2+1] = softClip(planar[n+i])
	}
	return &Buffer{Samples: out, Frames: n}, nil
}

// softClip 用 tanh 把样本约束在 (-1,1)，仅在接近/超过满量程时显著作用，
// 避免重采样过冲在写 16-bit WAV 时被硬削顶，保证浮点测量与落盘一致。
func softClip(v float32) float32 {
	av := v
	if av < 0 {
		av = -av
	}
	if av <= 0.99 {
		return v
	}
	return float32(math.Tanh(float64(v)))
}

// layoutFor 把声道数映射为标准布局。
func layoutFor(channels int) astiav.ChannelLayout {
	if channels <= 1 {
		return astiav.ChannelLayoutMono
	}
	return astiav.ChannelLayoutStereo
}

// writePlanar 把交错 float32 PCM 按 AVFrame 的 linesize 解交织写入各平面。
// 使用 SetBytes 提供按平面紧密排列的输入（每平面 n 个 float），
// FrameData 内部按 linesize 完成对齐拷贝。
func writePlanar(frame *astiav.Frame, f *packet.PCMFrame) error {
	n := f.NbSamples
	ch := f.Channels
	planar := make([]float32, n*ch)
	for c := 0; c < ch; c++ {
		for i := 0; i < n; i++ {
			planar[c*n+i] = f.Samples[i*ch+c]
		}
	}
	if err := frame.Data().SetBytes(float32Bytes(planar), 1); err != nil {
		return media.E(media.KindAudio, "audio.resample",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"写入平面输入帧失败", err)
	}
	return nil
}

func float32Bytes(s []float32) []byte {
	b := make([]byte, len(s)*4)
	for i, v := range s {
		bits := float32Bits(v)
		b[i*4] = byte(bits)
		b[i*4+1] = byte(bits >> 8)
		b[i*4+2] = byte(bits >> 16)
		b[i*4+3] = byte(bits >> 24)
	}
	return b
}

func bytesToFloat32(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := uint32(b[i*4]) | uint32(b[i*4+1])<<8 |
			uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
		out[i] = float32FromBits(bits)
	}
	return out
}
