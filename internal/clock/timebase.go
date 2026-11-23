// Package clock 定义全流水线共享的采样时间基准：采样率、声道数、位深，
// 以及字节偏移与采样帧序号、毫秒之间的确定性换算。
package clock

import (
	"errors"
	"fmt"
)

// Timebase 描述一路 PCM 素材的采样刻度。所有子系统共用同一实例，
// 以此建立唯一时间基准。
type Timebase struct {
	SampleRate    uint32 // 每秒采样帧数，例如 48000
	Channels      uint16 // 交织声道数，例如 2
	BitsPerSample uint16 // 每采样点位数，仅支持整数 PCM：8/16/24/32
}

// Valid 校验时间基准参数本身是否合法。
func (t Timebase) Valid() error {
	switch {
	case t.SampleRate == 0:
		return errors.New("clock: sample rate must not be zero")
	case t.Channels == 0:
		return errors.New("clock: channel count must not be zero")
	case t.BitsPerSample != 8 && t.BitsPerSample != 16 &&
		t.BitsPerSample != 24 && t.BitsPerSample != 32:
		return fmt.Errorf("clock: unsupported bits-per-sample %d", t.BitsPerSample)
	}
	return nil
}

// FrameSize 返回一个采样帧（所有声道各一个采样点）的字节数。
func (t Timebase) FrameSize() int {
	return int(t.Channels) * int(t.BitsPerSample) / 8
}

// FramesToBytes 将采样帧数换算为字节数。
func (t Timebase) FramesToBytes(frames uint64) int64 {
	return int64(frames) * int64(t.FrameSize())
}

// BytesToFrames 将字节偏移换算为采样帧序号；非整帧偏移一律拒绝，
// 并返回余数，调用方可据此报告精确的样本偏移。
func (t Timebase) BytesToFrames(byteOffset int64) (frames uint64, rem int, err error) {
	fs := int64(t.FrameSize())
	if byteOffset < 0 {
		return 0, 0, fmt.Errorf("clock: negative byte offset %d", byteOffset)
	}
	if r := byteOffset % fs; r != 0 {
		return 0, int(r), fmt.Errorf("clock: byte offset %d is not frame aligned (frame size %d)", byteOffset, fs)
	}
	return uint64(byteOffset / fs), 0, nil
}

// FramesToMillis 将采样帧时长换算为毫秒（向下取整）。
func (t Timebase) FramesToMillis(frames uint64) uint64 {
	if t.SampleRate == 0 {
		return 0
	}
	return frames * 1000 / uint64(t.SampleRate)
}

// MillisToFrames 将毫秒换算为采样帧数（向下取整）。
func (t Timebase) MillisToFrames(millis uint64) uint64 {
	if t.SampleRate == 0 {
		return 0
	}
	return millis * uint64(t.SampleRate) / 1000
}

// Summary 返回稳定的参数摘要文本，供检查点与参数指纹使用。
func (t Timebase) Summary() string {
	return fmt.Sprintf("%d/%d/%d", t.SampleRate, t.Channels, t.BitsPerSample)
}
