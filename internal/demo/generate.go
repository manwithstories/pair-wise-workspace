// Package demo 用确定性伪随机源在本地生成全部演示夹具：
// 正常素材、重复乱序 RTP 包、突发缺包、头部损坏、非整帧尾块与中断点。
// 不引用任何外部文件，相同 seed 必然产生逐字节一致的素材。
package demo

import (
	"encoding/binary"

	"media-pipeline/internal/clock"
)

// LCG 是数值配方里的线性同余生成器（Numerical Recipes 常数），
// 纯整数运算、无平台相关行为，保证跨运行确定性。
type LCG struct {
	state uint64
}

// NewLCG 以 seed 初始化生成器。
func NewLCG(seed uint64) *LCG {
	return &LCG{state: seed*6364136223846793005 + 1442695040888963407}
}

// Uint64 返回下一个伪随机 64 位值。
func (l *LCG) Uint64() uint64 {
	l.state = l.state*6364136223846793005 + 1442695040888963407
	return l.state
}

// Intn 返回 [0,n) 内的确定值。
func (l *LCG) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(l.Uint64() % uint64(n))
}

// Int16 返回一个伪随机带符号 16bit 采样。
func (l *LCG) Int16() int16 {
	return int16(l.Uint64() & 0xFFFF)
}

// Format 是演示素材统一使用的时间基准。
func Format() clock.Timebase {
	return clock.Timebase{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
}

// FramesPerPacket 是每个 RTP 包承载的固定帧数。
const FramesPerPacket = 4

// TonePCM 生成 n 帧立体声 16bit PCM：以低幅度底噪叠加周期性正弦包络，
// 其中刻意插入若干静音帧区间供静音检测识别。
func TonePCM(rng *LCG, n int, silentEvery int) []byte {
	tb := Format()
	buf := make([]byte, n*tb.FrameSize())
	for f := 0; f < n; f++ {
		var sample int16
		silent := silentEvery > 0 && f%silentEvery == 0
		if silent {
			sample = 0
		} else {
			// 周期性包络 + 小幅底噪，保证确定性且有明显响度起伏。
			env := int16(20000 + (f%37)*120)
			sample = env/2 + rng.Int16()/128
		}
		base := f * tb.FrameSize()
		binary.LittleEndian.PutUint16(buf[base:], uint16(sample))
		binary.LittleEndian.PutUint16(buf[base+2:], uint16(sample))
	}
	return buf
}

// CorruptByte 在指定字节处翻转低位，返回实际修改的偏移。
func CorruptByte(raw []byte, offset int) int {
	if offset < 0 || offset >= len(raw) {
		offset = len(raw) - 1
	}
	raw[offset] ^= 0x01
	return offset
}
