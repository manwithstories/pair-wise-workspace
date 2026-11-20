package fixture

import (
	"media-audit/internal/stream"
)

// Corruption 描述一次注入的损坏及其精确字节位置。
type Corruption struct {
	Offset int64             // 损坏字节的绝对偏移
	Kind   stream.IssueKind  // 期望被检出的错误类别
}

// InjectCorruptionAtFrame 在第 frameIndex 帧的语法单元头字节注入损坏，
// 返回损坏描述。头字节位置 = 该帧起始码之后第一个字节。
func InjectCorruptionAtFrame(codec stream.Codec, data []byte, frameIndex int) Corruption {
	sc := locateFrameHeader(data, frameIndex)
	switch codec {
	case stream.H264:
		// 置 forbidden_zero_bit=1。
		data[sc] |= 0x80
		return Corruption{Offset: sc, Kind: stream.IssueForbiddenNAL}
	case stream.H265:
		data[sc] |= 0x80
		return Corruption{Offset: sc, Kind: stream.IssueForbiddenNAL}
	case stream.AV1:
		// 置 obu_forbidden_bit=1。
		data[sc] |= 0x80
		return Corruption{Offset: sc, Kind: stream.IssueIllegalOBU}
	default:
		data[sc] |= 0x80
		return Corruption{Offset: sc, Kind: stream.IssueIllegalOBU}
	}
}

// locateFrameHeader 返回第 frameIndex 个帧级语法单元的头字节偏移。
// 参数集 NAL 与 AV1 序列头 OBU 不计入帧序号。起始码识别与
// internal/stream 的扫描器保持一致（支持 3/4 字节）。
func locateFrameHeader(data []byte, frameIndex int) int64 {
	var frameHeaders []int64
	n := len(data)
	pos := 0
	for pos+3 <= n {
		if data[pos] != 0 || data[pos+1] != 0 || data[pos+2] != 1 {
			pos++
			continue
		}
		// 头字节恒位于匹配三元组 00 00 01 之后（三元组位置 +3），
		// 无论前面是否还有一个零字节（4 字节起始码）。
		headerOff := pos + 3
		if headerOff < n && isFrameUnit(data[headerOff]) {
			frameHeaders = append(frameHeaders, int64(headerOff))
		}
		pos = headerOff + 1
	}
	if frameIndex >= len(frameHeaders) {
		frameIndex = len(frameHeaders) - 1
	}
	return frameHeaders[frameIndex]
}

// isFrameUnit 按头字节判断该单元是否承载帧（非参数集/序列头）。
func isFrameUnit(h byte) bool {
	switch h {
	case 0x67, 0x68: // H.264 SPS/PPS
		return false
	case 0x42, 0x44, 0x46: // H.265 VPS/SPS/PPS
		return false
	case 0x0A: // AV1 sequence header OBU
		return false
	default:
		return true
	}
}

// WrapTimestamps 生成一条在 wrapAt 帧处跨越 32 位边界的时间戳序列。
// 起点取 2^32 减去若干步长，使第 wrapAt 帧的裸值自然回绕到小值。
func WrapTimestamps(frames []FrameTS, start uint32, step uint32, wrapAt int) {
	base := uint64(0x100000000) - uint64(step)*uint64(wrapAt)
	for i := range frames {
		frames[i].Raw = uint32(base + uint64(step)*uint64(i))
	}
}

// FrameTS 是时间戳场景辅助的最小帧结构。
type FrameTS struct{ Raw uint32 }

// TruncateStream 在字节偏移 cutAt 处截断码流，并在截断点追加两个零字节
// 模拟未写完的起始码（写入中断的典型形态）。返回截断后的数据。
func TruncateStream(data []byte, cutAt int) []byte {
	cut := data[:cutAt]
	return append(append([]byte{}, cut...), 0x00, 0x00)
}

// DriftPattern 生成每帧滴答步长，ppm 为期望漂移（百万分比）。
func DriftPattern(base uint32, ppm float64, n int) []uint32 {
	out := make([]uint32, n)
	scaled := float64(base) * (1 + ppm/1_000_000)
	for i := range out {
		out[i] = uint32(scaled + 0.5)
	}
	return out
}

// VFRPattern 生成交替长短帧步长，longRatio 控制长帧相对名义步长的倍数。
func VFRPattern(base uint32, longRatio float64) []uint32 {
	return []uint32{base, base, base, uint32(float64(base)*longRatio + 0.5)}
}
