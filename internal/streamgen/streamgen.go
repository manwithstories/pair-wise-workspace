// Package streamgen 生成自检所需的跨节点包流夹具：含音频 RTP、视频时间标记、
// RTCP Sender Report，以及缺包、乱序、时间戳回绕、150ppm 漂移、VFR、截断包、
// 坏负载等受控故障。全部数据由种子确定性生成，不引用外部文件。
package streamgen

import (
	"encoding/binary"
	"math/rand"
)

// Spec 描述一条待生成的包流。
type Spec struct {
	Seed        int64
	DurationMS  int
	AudioRate   int // L16 采样率
	Channels    int
	ClockRate   int // RTP 时钟基率（通常等于 AudioRate）
	PPM         int // 注入漂移
	DamageSeq   uint16 // 注入坏负载的序号；^uint16(0) 表示不注入
	DamageKind  DamageKind
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
	// 预留回绕点：从接近 32 位边界开始，使长流必然跨越一次回绕。
	ts = 0xFFFFFFFF - uint32(s.AudioRate*2)
	const startSeq = 100
	var buf []byte

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

		// 受控乱序：把序号 500 与 501 的写出顺序对调。
		if uint16(seq) == 500 {
			defered := pkt
			next := buildAudio(seq+1, ts+inc, s.Channels, frameBytes, rng)
			buf = append(buf, frame(next)...)
			buf = append(buf, frame(defered)...)
			audioSeqs = append(audioSeqs, 501, 500)
			i++ // 跳过下一轮的 501
			continue
		}

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

		// 每 1 秒插入一条 RTCP SR，墙钟按漂移后的真实时间推进。
		if i > 0 && i%100 == 0 {
			sr := buildSR(ntpFor(i, s.PPM), ts-inc)
			buf = append(buf, frame(sr)...)
		}
	}
	return buf, audioSeqs
}

// buildAudio 构造 L16 RTP 包（PT=97）。
func buildAudio(seq, ts uint32, chans, frameBytes int, rng *rand.Rand) []byte {
	h := make([]byte, 12)
	h[0] = 0x80 // V=2
	h[1] = 97
	binary.BigEndian.PutUint16(h[2:4], uint16(seq))
	binary.BigEndian.PutUint32(h[4:8], ts)
	binary.BigEndian.PutUint32(h[8:12], 0xA1B2C3D4)
	payload := make([]byte, frameBytes)
	// 确定性的低峰值正弦 + 噪声，保证响度有提升空间。
	for i := range payload {
		payload[i] = byte(rng.Intn(64))
	}
	return append(h, payload...)
}

// buildSR 构造 RTCP Sender Report。
func buildSR(ntpSec, rtpTS uint32) []byte {
	b := make([]byte, 28)
	b[0] = 0x80
	b[1] = 200
	binary.BigEndian.PutUint16(b[2:4], 6)
	binary.BigEndian.PutUint32(b[8:12], ntpSec)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	return b
}

// applyDamage 按类别破坏数据包。
func applyDamage(pkt []byte, kind DamageKind) []byte {
	switch kind {
	case DamageBadPayload:
		pkt[0] = 0x00 // 非法版本号
		return pkt
	case DamageTruncated:
		// 声明一个远大于实际负载的长度，制造分帧故障。
		out := make([]byte, 4+len(pkt))
		binary.BigEndian.PutUint32(out, uint32(len(pkt)+50))
		copy(out[4:], pkt)
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

// ntpFor 把第 i 个包对应的秒数换算为 NTP 秒（1900 纪元）。
func ntpFor(i, ppm int) uint32 {
	const ntpEpochOffset = 2208988800
	secs := i / 100
	return uint32(ntpEpochOffset + 1000 + secs)
}
