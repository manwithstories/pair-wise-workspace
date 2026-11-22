// Package sample 用确定性伪随机过程生成全部演示与测试夹具：
// PCM/IEEE-float WAV、RTP 包序列与分片 MP4，并可注入截断、时间戳回退、
// 突发丢包与重复冲突。相同种子与参数必须产生逐字节一致的输出。
package sample

import (
	"encoding/binary"
	"math/rand"

	"github.com/pion/rtp"
)

// 固定的媒体参数，保证时间基准明确。
const (
	AudioRate     uint32 = 48000
	VideoRate     uint32 = 90000
	Channels      uint16 = 2
	BitsPerSample uint16 = 16
	FrameBytes           = int(Channels) * int(BitsPerSample) / 8 // 4 字节/帧
	// 每个 RTP 音频包携带的帧数（10ms @48kHz）。
	FramesPerPacket = 480
	PacketSamples   = uint32(FramesPerPacket) // 单声道刻度下的样本增量
)

// Options 控制样例生成。
type Options struct {
	Seed          int64
	Packets       int    // 生成的 RTP 包总数
	LossBurst     int    // 突发连续丢包数（0 表示不丢）
	LossAt        int    // 突发丢包起始的包序号位置
	TruncateWAV   bool   // 截断 WAV data 块
	TimestampBack bool   // 注入一次时间戳回退
	DupConflict   bool   // 注入同序号不同载荷冲突
	Redundancy    int    // 为前 N 个丢失包提供一致冗余
}

// Bundle 是一次生成的完整夹具。
type Bundle struct {
	WAV          []byte
	Packets      []*rtp.Packet // 按“到达顺序”（可乱序）排列
	ExpectFrames int           // WAV 应包含的完整帧数
	ExpectSeqs   []uint16      // 期望覆盖的连续序号集合（无丢失视角）
	Redundant    map[uint16][]byte
	LostSeqs     []uint16
}

// pcg 风格不需要：直接用 math/rand 的确定性源（同种子同序列）。
// Generate 按选项产出夹具，整个过程不依赖时间或外部状态。
func Generate(o Options) *Bundle {
	rng := rand.New(rand.NewSource(o.Seed))
	b := &Bundle{Redundant: map[uint16][]byte{}}

	// 1) 生成连续序号与单调时间戳的音频包。
	total := o.Packets
	if total <= 0 {
		total = 64
	}
	baseSeq := uint16(0)
	ts := uint32(0)
	type item struct {
		seq     uint16
		ts      uint32
		payload []byte
	}
	all := make([]item, 0, total)
	for i := 0; i < total; i++ {
		payload := make([]byte, FramesPerPacket*FrameBytes)
		for k := range payload {
			payload[k] = byte(rng.Intn(256))
		}
		all = append(all, item{
			seq: baseSeq + uint16(i),
			ts:  ts,
			payload: payload,
		})
		ts += PacketSamples
		b.ExpectSeqs = append(b.ExpectSeqs, baseSeq+uint16(i))
	}
	b.ExpectFrames = total * FramesPerPacket

	// 2) 注入时间戳回退（在乱序之前作用于逻辑序号，确保按序核对可见）。
	if o.TimestampBack && total > 2 {
		all[1].ts = all[0].ts - PacketSamples
	}

	// 3) 注入突发丢失。
	lossStart := o.LossAt
	if lossStart <= 0 || lossStart >= total {
		lossStart = total / 2
	}
	lossEnd := lossStart + o.LossBurst
	if lossEnd > total {
		lossEnd = total
	}
	lostSet := map[uint16]bool{}
	for i := lossStart; i < lossEnd; i++ {
		lostSet[all[i].seq] = true
		b.LostSeqs = append(b.LostSeqs, all[i].seq)
	}
	// 为前 Redundancy 个丢失包生成“校验一致”的冗余副本。
	for i, seq := range b.LostSeqs {
		if i >= o.Redundancy {
			break
		}
		b.Redundant[seq] = append([]byte(nil), all[lossStart+i].payload...)
	}

	// 3) 组装到达序列：跳过丢失包，并制造确定性乱序（相邻交换）。
	arrival := make([]item, 0, total)
	for _, it := range all {
		if lostSet[it.seq] {
			continue
		}
		arrival = append(arrival, it)
	}
	// 确定性乱序：对偶数位置与下一位置交换（若都存在且未丢失）。
	for i := 0; i+1 < len(arrival); i += 2 {
		if rng.Intn(2) == 0 {
			arrival[i], arrival[i+1] = arrival[i+1], arrival[i]
		}
	}
	// 重复冲突：复制一个已到达包但更换载荷。
	conflictSeq := uint16(0xFFFF)
	if o.DupConflict && len(arrival) > 0 {
		conflictSeq = arrival[0].seq
	}
	for _, it := range arrival {
		pkt := &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: it.seq, Timestamp: it.ts, PayloadType: 96},
			Payload: append([]byte(nil), it.payload...),
		}
		b.Packets = append(b.Packets, pkt)
		if it.seq == conflictSeq {
			bad := append([]byte(nil), it.payload...)
			bad[0] ^= 0xFF // 同序号、不同载荷
			b.Packets = append(b.Packets, &rtp.Packet{
				Header:  rtp.Header{SequenceNumber: it.seq, Timestamp: it.ts, PayloadType: 96},
				Payload: bad,
			})
		}
	}

	// 4) 用未丢失的原始 PCM 拼出 WAV（截断选项作用于 data 长度）。
	var pcm []byte
	for _, it := range all {
		if lostSet[it.seq] {
			continue
		}
		pcm = append(pcm, it.payload...)
	}
	b.WAV = buildWAV(pcm, o.TruncateWAV)
	return b
}

// buildWAV 构造 16-bit 双声道 PCM WAV；truncate 时砍掉末尾若干帧并谎报长度。
func buildWAV(pcm []byte, truncate bool) []byte {
	data := pcm
	if truncate && len(data) > FrameBytes*4 {
		data = data[:len(data)-FrameBytes*3] // 留下悬空/缺失尾部
	}
	riffSize := uint32(36 + len(data))
	out := make([]byte, 0, 44+len(data))
	out = append(out, []byte("RIFF")...)
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], riffSize)
	out = append(out, tmp[:]...)
	out = append(out, []byte("WAVE")...)
	// fmt
	out = append(out, []byte("fmt ")...)
	binary.LittleEndian.PutUint32(tmp[:], 16)
	out = append(out, tmp[:]...)
	var body [16]byte
	binary.LittleEndian.PutUint16(body[0:2], 1) // PCM
	binary.LittleEndian.PutUint16(body[2:4], Channels)
	binary.LittleEndian.PutUint32(body[4:8], AudioRate)
	blockAlign := uint16(FrameBytes)
	avg := AudioRate * uint32(blockAlign)
	binary.LittleEndian.PutUint32(body[8:12], avg)
	binary.LittleEndian.PutUint16(body[12:14], blockAlign)
	binary.LittleEndian.PutUint16(body[14:16], BitsPerSample)
	out = append(out, body[:]...)
	// data
	out = append(out, []byte("data")...)
	declared := len(data)
	if truncate {
		// 声明长度大于实际，制造 data 截断。
		declared += FrameBytes * 2
	}
	binary.LittleEndian.PutUint32(tmp[:], uint32(declared))
	out = append(out, tmp[:]...)
	out = append(out, data...)
	return out
}
