package audio

import (
	"fmt"

	"github.com/asticode/go-astiav"
)

// Normalizer 用 libswresample 把任意支持的输入规格统一为 48kHz 双声道 fltp。
// 内部维护一个可复用的输出帧，按输入帧动态设置样本数，调用方用完即取走字节。
type Normalizer struct {
	swr      *astiav.SoftwareResampleContext
	srcRate  int
	srcChans int
}

// NewNormalizer 构造重采样器；输出规格固定 48kHz/stereo。
func NewNormalizer(srcRate, srcChans int) (*Normalizer, error) {
	if _, err := NewDecoder(srcRate, srcChans); err != nil {
		return nil, err
	}
	swr := astiav.AllocSoftwareResampleContext()
	if swr == nil {
		return nil, fmt.Errorf("alloc swresample context failed")
	}
	return &Normalizer{swr: swr, srcRate: srcRate, srcChans: srcChans}, nil
}

// Free 释放底层 SWR 资源。
func (n *Normalizer) Free() {
	if n.swr != nil {
		n.swr.Free()
	}
}

// Process 把一帧 s16 交错输入转换为 48k/stereo/fltp 帧。
// 返回的帧归调用方所有，必须 Free。
func (n *Normalizer) Process(src *astiav.Frame) (*astiav.Frame, error) {
	// 输出样本数按采样率比例上取整，SWR 会在 Flush 时吐出延迟样本。
	outSamples := avRescaleDelayed(n.swr, src.NbSamples(), n.srcRate)
	dst := astiav.AllocFrame()
	if dst == nil {
		return nil, fmt.Errorf("alloc dst frame failed")
	}
	dst.SetSampleFormat(astiav.SampleFormatFltp)
	dst.SetSampleRate(TargetRate)
	dst.SetChannelLayout(astiav.ChannelLayoutStereo)
	dst.SetNbSamples(outSamples)
	if err := dst.AllocBuffer(0); err != nil {
		dst.Free()
		return nil, fmt.Errorf("alloc dst buffer: %w", err)
	}
	if err := n.swr.ConvertFrame(src, dst); err != nil {
		dst.Free()
		return nil, fmt.Errorf("swr convert: %w", err)
	}
	return dst, nil
}

// Flush 取出 SWR 内部缓冲的延迟样本（流结束时调用）；无延迟返回 nil。
func (n *Normalizer) Flush() (*astiav.Frame, error) {
	delay := n.swr.Delay(TargetRate)
	if delay <= 0 {
		return nil, nil
	}
	dst := astiav.AllocFrame()
	if dst == nil {
		return nil, fmt.Errorf("alloc flush frame failed")
	}
	dst.SetSampleFormat(astiav.SampleFormatFltp)
	dst.SetSampleRate(TargetRate)
	dst.SetChannelLayout(astiav.ChannelLayoutStereo)
	dst.SetNbSamples(int(delay))
	if err := dst.AllocBuffer(0); err != nil {
		dst.Free()
		return nil, fmt.Errorf("alloc flush buffer: %w", err)
	}
	if err := n.swr.ConvertFrame(nil, dst); err != nil {
		dst.Free()
		return nil, fmt.Errorf("swr flush: %w", err)
	}
	if dst.NbSamples() == 0 {
		dst.Free()
		return nil, nil
	}
	return dst, nil
}

// avRescaleDelayed 估算给定输入样本对应的输出样本数，含 SWR 已缓冲延迟。
func avRescaleDelayed(swr *astiav.SoftwareResampleContext, inSamples, srcRate int) int {
	delay := swr.Delay(TargetRate)
	scaled := int64(inSamples)*int64(TargetRate)/int64(srcRate) + delay + 3
	return int(scaled)
}

// FrameToInterleavedS16 把 fltp 帧打包成交错 s16 小端字节（交付 PCM 规格）。
func FrameToInterleavedS16(f *astiav.Frame) ([]byte, error) {
	// 借 S16 格式帧做一次输出转换：fltp -> s16 interleaved。
	out := astiav.AllocFrame()
	if out == nil {
		return nil, fmt.Errorf("alloc s16 frame failed")
	}
	defer out.Free()
	out.SetSampleFormat(astiav.SampleFormatS16)
	out.SetSampleRate(f.SampleRate())
	out.SetChannelLayout(f.ChannelLayout())
	out.SetNbSamples(f.NbSamples())
	if err := out.AllocBuffer(0); err != nil {
		return nil, err
	}
	swr := astiav.AllocSoftwareResampleContext()
	defer swr.Free()
	if err := swr.ConvertFrame(f, out); err != nil {
		return nil, err
	}
	b, err := out.Data().Bytes(0)
	if err != nil {
		return nil, err
	}
	return b, nil
}
