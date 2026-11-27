// Package streamgen 生成自检所需的跨节点包流夹具：含音频 RTP、视频时间标记、
// RTCP Sender Report，以及缺包、乱序、时间戳回绕、150ppm 漂移、VFR、截断包、
// 坏负载等受控故障。全部数据由种子确定性生成，不引用外部文件。
package streamgen

import (
	"encoding/binary"
	"math"
	"math/rand"
)

// Spec 描述一条待生成的包流。
type Spec struct {
	Seed       int64
	DurationMS int
	AudioRate  int // L16 采样率
	Channels   int
	ClockRate  int // RTP 时钟基率（通常等于 AudioRate）
	PPM        int // 注入漂移
	DamageSeq  uint16 // 注入坏负载的序号；^uint16(0) 表示不注入
	DamageKind DamageKind
	// WrapStart 为 true 时让 RTP 时间戳从接近 32 位边界开始，
	// 使流必然跨越一次回绕，覆盖时间戳回绕诊断。
	WrapStart bool
}

// DamageKind 选择坏包的破坏方式。
type DamageKind uint8

const (
	DamageNone DamageKind = iota
	DamageBadPayload // 破坏 RTP 版本号，触发坏负载故障
	DamageTruncated  // 长度前缀与实际不符，触发截断/分帧故障
)

// 音频包周期：每包 10ms。
const packetMS = 10

// Packet 是生成器产出的一条带长度前缀的记录。
type Packet struct {
	Seq     uint16
	IsRTCP  bool
	IsVideo bool
	Raw     []byte // 含 4 字节长度前缀的完整记录
}

// Generate 确定性生成包流。返回的字节可直接写入 .bin 夹具，
// meta 返回每个音频包的序号，便于自检核对坏包定位。
func Generate(s Spec) (stream []byte, audioSeqs []uint16) {
	rng := rand.New(rand.NewSource(s.Seed))
	nPackets := s.DurationMS / packetMS
	samplesPerPacket := s.AudioRate / 100
	frameBytes := samplesPerPacket * s.Channels * 2 // L16

	var ts uint32
	const startSeq = 100
	var buf []byte
	var startTS uint32
	if s.WrapStart {
		// 距 32 位边界 2 秒，前几百个包即可触发一次回绕。
		startTS = 0xFFFFFFFF - uint32(s.ClockRate*2)
	}
	ts = startTS
	// SR 锚点时钟：与包序号 i 严格对齐的漂移 RTP 时间戳，
	// 基于固定起始基准，不受循环 ts 推进或缺包/乱序写出顺序影响。
	srTS := func(i int) uint32 {
		base := scaleWithPPM(uint64(s.ClockRate/100)*uint64(i), s.PPM)
		return startTS + uint32(base)
	}

	for i := 0; i < nPackets; i++ {
		seq := uint32(startSeq + i)
		// 漂移：发送端偏快 PPM，时间戳增量按比例放大。
		inc := uint32(scaleWithPPM(uint64(s.ClockRate/100), s.PPM))
		ts += inc

		// 受控缺包：跳过序号 300（不写入流）。
		if seq == 300 {
			continue
		}

		pkt := buildAudio(seq, ts, s.Channels, frameBytes, rng)

		// 受控乱序：把序号 500 与 501 的写出顺序对调（501 先写、500 后写），
		// 但两者时间戳仍严格递增，只改变到达顺序。
		if uint16(seq) == 500 {
			ts500 := ts
			ts501 := ts + inc // 501 的时间戳比 500 多一个增量
			next := buildAudio(seq+1, ts501, s.Channels, frameBytes, rng)
			defered := buildAudio(seq, ts500, s.Channels, frameBytes, rng)
			buf = append(buf, frame(next)...)
			buf = append(buf, frame(defered)...)
			audioSeqs = append(audioSeqs, 501, 500)
			ts = ts501 // 让主循环的 ts 与已消费的两个增量对齐
			i++        // 跳过下一轮的 501
			continue
		}
		_ = pkt

		// 注入坏包。
		if uint16(seq) == s.DamageSeq {
			if s.DamageKind == DamageTruncated {
				// 声明大于实际的长度，制造分帧故障（自带长度前缀）。
				buf = append(buf, applyDamage(pkt, s.DamageKind)...)
				audioSeqs = append(audioSeqs, uint16(seq))
				continue
			}
			pkt = applyDamage(pkt, s.DamageKind)
		}

		buf = append(buf, frame(pkt)...)
		audioSeqs = append(audioSeqs, uint16(seq))

		// 每 0.1 秒插入一条 RTCP SR，提供密集锚点以估计 ppm。
		// RTP 字段是与包序号严格配对的漂移时间戳；NTP 走真实墙钟（不含漂移）。
		// 恢复器测锚点间 RTP 标称跨度 / NTP 真实跨度的速率比即可恢复 ppm。
		if i > 0 && i%10 == 0 {
			ntpSec, ntpFrac := wallNTP(i)
			sr := buildSR(ntpSec, ntpFrac, srTS(i))
			buf = append(buf, frame(sr)...)
		}	}
	return buf, audioSeqs
}

// buildAudio 构造 L16 RTP 包（PT=97）。
// 信号为低峰值确定性正弦（约 -23 LUFS），给响度归一留出向 -16 提升的空间。
func buildAudio(seq, ts uint32, chans, frameBytes int, rng *rand.Rand) []byte {
	h := make([]byte, 12)
	h[0] = 0x80 // V=2
	h[1] = 97
	binary.BigEndian.PutUint16(h[2:4], uint16(seq))
	binary.BigEndian.PutUint32(h[4:8], ts)
	binary.BigEndian.PutUint32(h[8:12], 0xA1B2C3D4)
	samples := frameBytes / 2
	payload := make([]byte, frameBytes)
	// 峰值 0.18 的 440Hz 正弦；各声道幅度略异，保证下混后双声道都有能量。
	const peak = 0.18
	for i := 0; i < samples; i++ {
		ch := i % chans
		amp := peak * (1.0 - 0.2*float64(ch))
		v := amp * math.Sin(2*math.Pi*440*float64(i/chans)/48000)
		s := int16(v * math.MaxInt16)
		binary.LittleEndian.PutUint16(payload[i*2:], uint16(s))
	}
	return append(h, payload...)
}

// buildSR 构造 RTCP Sender Report，含 NTP 秒与 32 位小数部分。
func buildSR(ntpSec, ntpFrac, rtpTS uint32) []byte {
	b := make([]byte, 28)
	b[0] = 0x80
	b[1] = 200
	binary.BigEndian.PutUint16(b[2:4], 6)
	binary.BigEndian.PutUint32(b[8:12], ntpSec)
	binary.BigEndian.PutUint32(b[12:16], ntpFrac)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	return b
}

// wallNTP 把第 i 个包的真实墙钟时刻（每包 10ms）换算为 NTP 秒+小数。
func wallNTP(i int) (sec, frac uint32) {
	const ntpEpochOffset = 2208988800
	nanos := int64(i) * 10_000_000 // 10ms
	sec = uint32(ntpEpochOffset+1000) + uint32(nanos/1_000_000_000)
	remainder := nanos % 1_000_000_000
	frac = uint32(remainder * (1 << 32) / 1_000_000_000)
	return sec, frac
}

// applyDamage 按类别破坏数据包。
func applyDamage(pkt []byte, kind DamageKind) []byte {
	switch kind {
	case DamageBadPayload:
		pkt[0] = 0x00 // 非法版本号
		return pkt
	case DamageTruncated:
		// 声明小于 RTP 最小头部（12）的非法长度，扫描器无法定位边界，
		// 直接判定为致命分帧故障（退出码 2）。
		out := make([]byte, 4)
		binary.BigEndian.PutUint32(out, 6)
		return out
	default:
		return pkt
	}
}

// frame 添加 4 字节大端长度前缀。
func frame(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

// scaleWithPPM 按 ppm 调整时间戳增量（正值=偏快=增量放大）。
func scaleWithPPM(base uint64, ppm int) uint64 {
	return base * uint64(1_000_000+ppm) / 1_000_000
}
