package fixture

import (
	"math/rand"
	"time"

	pionrtp "github.com/pion/rtp"

	"mediakernel/internal/clock"
)

// 传输参数。
const (
	MTU                 = 1200
	payloadHeader       = 4 // frameIdx(2) + frag(1) + frags(1)
	VideoClockRate      = 90000
	AudioClockRate      = 48000
	VideoSSRC           = 0x1001
	AudioSSRC           = 0x1002
	RetransmitCapacity  = 4 // 重传缓存容量（每轨）
	jitterBaseMs        = 5
	jitterSpreadMs      = 25
	spikeProbability    = 0.02
	spikeExtraMs        = 120
	lossBurstStartPct   = 45 // 突发丢包起点（全局序号百分比）
)

// Packet 是一个待传输的 RTP 包及其网络轨迹。
type Packet struct {
	RTP     *pionrtp.Packet
	Track   uint64
	Frame   int
	Send    clock.Nanos
	Arrival clock.Nanos // 计划到达时刻
	Lost    bool        // 突发丢包中丢失
}

// Packetize 把真值块切分为 RTP 包并生成确定性网络轨迹
// （抖动、尖峰、突发丢包）。
func Packetize(blocks []Block, p Params) []*Packet {
	chunk := MTU - payloadHeader
	seq := map[uint64]uint16{1: uint16(p.Seed), 2: uint16(p.Seed) + 5000}
	var pkts []*Packet
	for _, blk := range blocks {
		frags := (len(blk.Data) + chunk - 1) / chunk
		rate := uint32(VideoClockRate)
		ssrc := uint32(VideoSSRC)
		if blk.Track == 2 {
			rate = AudioClockRate
			ssrc = AudioSSRC
		}
		ts := uint32(uint64(blk.AbsNs) * uint64(rate) / 1_000_000_000)
		for f := 0; f < frags; f++ {
			end := (f + 1) * chunk
			if end > len(blk.Data) {
				end = len(blk.Data)
			}
			pl := make([]byte, 0, payloadHeader+end-f*chunk)
			pl = append(pl, byte(blk.Index>>8), byte(blk.Index), byte(f), byte(frags))
			pl = append(pl, blk.Data[f*chunk:end]...)
			pkt := &pionrtp.Packet{Header: pionrtp.Header{
				Version:        2,
				Marker:         blk.Track == 1 && f == frags-1,
				PayloadType:    96,
				SequenceNumber: seq[blk.Track],
				Timestamp:      ts,
				SSRC:           ssrc,
			}, Payload: pl}
			seq[blk.Track]++
			pkts = append(pkts, &Packet{
				RTP: pkt, Track: blk.Track, Frame: blk.Index,
				Send: clock.Nanos(blk.AbsNs),
			})
		}
	}
	// 网络轨迹：基础抖动 + 偶发尖峰
	rng := rand.New(rand.NewSource(p.Seed*7919 + 1))
	for _, pkt := range pkts {
		jitter := time.Duration(jitterBaseMs+rng.Int63n(jitterSpreadMs)) * time.Millisecond
		if rng.Float64() < spikeProbability {
			jitter += spikeExtraMs * time.Millisecond
		}
		pkt.Arrival = pkt.Send + clock.Nanos(jitter)
	}
	// 突发丢包：全局发送序中的一段
	start := len(pkts) * lossBurstStartPct / 100
	for i := start; i < start+p.LossBurst && i < len(pkts); i++ {
		pkts[i].Lost = true
	}
	return pkts
}
