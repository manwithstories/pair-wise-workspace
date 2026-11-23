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
func marshalPacket(seq uint16, rng *LCG, withTone bool) RTPDatagram {
	tb := Format()
	var pcm []byte
	if withTone {
		pcm = TonePCM(rng, FramesPerPacket, 0)
	} else {
		pcm = make([]byte, FramesPerPacket*tb.FrameSize())
	}
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * uint32(FramesPerPacket),
			SSRC:           0x42424242,
		},
		Payload: pcm,
	}
	raw, _ := p.Marshal()
	return RTPDatagram{Seq: seq, Bytes: raw}
}

// BuildScenarios 用给定 seed 装配全部场景，顺序固定，输出确定。
func BuildScenarios(seed uint64) []Scenario {
	const packetCount = 24

	out := make([]Scenario, 0, 6)

	// 1. 正常素材：严格递增、无重复无缺号。
	normal := Scenario{Name: ScenNormal, BlockID: "blk-normal"}
	for i := 0; i < packetCount; i++ {
		normal.Datagrams = append(normal.Datagrams, marshalPacket(uint16(i), NewLCG(seed+uint64(i)), true))
	}
	out = append(out, normal)

	// 2. 重复 + 乱序：交换相邻包并重复其中一个。
	reorder := Scenario{Name: ScenReorder, BlockID: "blk-reorder"}
	for i := 0; i < packetCount; i++ {
		reorder.Datagrams = append(reorder.Datagrams, marshalPacket(uint16(i), NewLCG(seed+100+uint64(i)), true))
	}
	reorder.Datagrams[4], reorder.Datagrams[5] = reorder.Datagrams[5], reorder.Datagrams[4]
	reorder.Datagrams = append(reorder.Datagrams, reorder.Datagrams[6]) // 重复包
	out = append(out, reorder)

	// 3. 突发缺包：序号 10..12 整体缺失，后续包正常到达。
	burst := Scenario{Name: ScenBurstLoss, BlockID: "blk-burst", ExpectGap: true}
	for i := 0; i < packetCount; i++ {
		if i >= 10 && i <= 12 {
			continue // 模拟突发丢包
		}
		burst.Datagrams = append(burst.Datagrams, marshalPacket(uint16(i), NewLCG(seed+200+uint64(i)), true))
	}
	out = append(out, burst)

	// 4. 头部损坏：素材正常但在 WAV data 区内预置一个损坏字节。
	bad := Scenario{Name: ScenBadHeader, BlockID: "blk-bad", InjectOffset: 64}
	for i := 0; i < packetCount; i++ {
		bad.Datagrams = append(bad.Datagrams, marshalPacket(uint16(i), NewLCG(seed+300+uint64(i)), true))
	}
	out = append(out, bad)

	// 5. 非整帧尾块：正常包之后附加无法成帧的 3 个字节。
	un := Scenario{Name: ScenUnaligned, BlockID: "blk-unalign", Trailing: 3}
	for i := 0; i < packetCount; i++ {
		un.Datagrams = append(un.Datagrams, marshalPacket(uint16(i), NewLCG(seed+400+uint64(i)), true))
	}
	out = append(out, un)

	// 6. 中断续跑：更长的正常素材，供检查点机制演示中断与恢复。
	res := Scenario{Name: ScenResume, BlockID: "blk-resume"}
	for i := 0; i < packetCount*2; i++ {
		res.Datagrams = append(res.Datagrams, marshalPacket(uint16(i), NewLCG(seed+500+uint64(i)), true))
	}
	out = append(out, res)

	return out
}

// HashBytes 返回输入的 SHA-256，供源块身份与检查点核对。
func HashBytes(b []byte) [32]byte { return sha256.Sum256(b) }
