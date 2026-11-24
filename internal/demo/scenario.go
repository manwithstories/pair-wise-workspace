package demo

import (
	"crypto/sha256"

	"github.com/pion/rtp"
)

// Scenario 名称常量，对应验收要求的六类样例。
const (
	ScenNormal      = "normal"
	ScenReorder     = "reorder"
	ScenBurstLoss   = "burst-loss"
	ScenBadHeader   = "bad-header"
	ScenUnaligned   = "unaligned-tail"
	ScenResume      = "resume"
)

// RTPDatagram 是一个已序列化的 RTP 捕获数据报及其序号。
type RTPDatagram struct {
	Seq   uint16
	Bytes []byte
}

// Scenario 描述一个装配好的演示场景。
type Scenario struct {
	Name       string
	BlockID    string
	Datagrams  []RTPDatagram // 按发送顺序（可乱序、可重复、可缺号）
	ExpectGap  bool          // 是否预期产生缺口
	// 损坏注入：对最终 WAV 字节在 InjectOffset 处翻转。
	InjectOffset int
	// 非整帧尾块：附加 Trailing 个无法成帧的字节。
	Trailing int
}

// marshalPacket 构造固定载荷帧数的 RTP 包并序列化。
func marshalPacket(seq uint16, payload []byte) RTPDatagram {
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * uint32(FramesPerPacket),
			SSRC:           0x42424242,
		},
		Payload: payload,
	}
	raw, _ := p.Marshal()
	return RTPDatagram{Seq: seq, Bytes: raw}
}

// streamPackets 生成 totalPackets 个包对应的一条连续 PCM 流，
// 静音模式在整条流上连续分布，不被 4 帧的包边界打断，再按包切分。
func streamPackets(seed, salt uint64, totalPackets int) []RTPDatagram {
	tb := Format()
	totalFrames := totalPackets * FramesPerPacket
	stream := TonePCM(NewLCG(seed+salt), totalFrames, 0)
	pkts := make([]RTPDatagram, 0, totalPackets)
	for i := 0; i < totalPackets; i++ {
		off := i * FramesPerPacket * tb.FrameSize()
		end := off + FramesPerPacket*tb.FrameSize()
		pkts = append(pkts, marshalPacket(uint16(i), stream[off:end]))
	}
	return pkts
}

// BuildScenarios 用给定 seed 装配全部场景，顺序固定，输出确定。
func BuildScenarios(seed uint64) []Scenario {
	// 48 帧/包 × 64 包 = 3072 帧（64ms），足以包含多个完整 48 帧静音段。
	const packetCount = 64

	out := make([]Scenario, 0, 6)

	// 1. 正常素材：严格递增、无重复无缺号。
	normal := Scenario{Name: ScenNormal, BlockID: "blk-normal"}
	normal.Datagrams = streamPackets(seed, 0, packetCount)
	out = append(out, normal)

	// 2. 重复 + 乱序：交换相邻包并重复其中一个。
	reorder := Scenario{Name: ScenReorder, BlockID: "blk-reorder"}
	reorder.Datagrams = streamPackets(seed, 100, packetCount)
	reorder.Datagrams[4], reorder.Datagrams[5] = reorder.Datagrams[5], reorder.Datagrams[4]
	reorder.Datagrams = append(reorder.Datagrams, reorder.Datagrams[6]) // 重复包
	out = append(out, reorder)

	// 3. 突发缺包：序号 10..12 整体缺失，后续包正常到达。
	burst := Scenario{Name: ScenBurstLoss, BlockID: "blk-burst", ExpectGap: true}
	for _, pkt := range streamPackets(seed, 200, packetCount) {
		if pkt.Seq >= 10 && pkt.Seq <= 12 {
			continue // 模拟突发丢包
		}
		burst.Datagrams = append(burst.Datagrams, pkt)
	}
	out = append(out, burst)

	// 4. 头部损坏：素材正常但在 fmt 块 blockAlign 字段（文件偏移 32）预置损坏。
	//    这是结构性头部损坏，解码器必报且能精确定位到字节。
	bad := Scenario{Name: ScenBadHeader, BlockID: "blk-bad", InjectOffset: 32}
	bad.Datagrams = streamPackets(seed, 300, packetCount)
	out = append(out, bad)

	// 5. 非整帧尾块：正常包之后附加无法成帧的 3 个字节。
	un := Scenario{Name: ScenUnaligned, BlockID: "blk-unalign", Trailing: 3}
	un.Datagrams = streamPackets(seed, 400, packetCount)
	out = append(out, un)

	// 6. 中断续跑：更长的正常素材，供检查点机制演示中断与恢复。
	res := Scenario{Name: ScenResume, BlockID: "blk-resume"}
	res.Datagrams = streamPackets(seed, 500, packetCount*2)
	out = append(out, res)

	return out
}

// HashBytes 返回输入的 SHA-256，供源块身份与检查点核对。
func HashBytes(b []byte) [32]byte { return sha256.Sum256(b) }
