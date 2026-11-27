package pipeline

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"

	"github.com/go-audio/riff"

	"bcastqc/internal/ingest"
)

// sampleRate 为夹具统一使用的采样率。
const sampleRate = 48000

// TrackFixture 是单条轨道的全部生成数据。
type TrackFixture struct {
	ID          int
	WAV         []byte
	WAVSize     int64
	Info        *ingest.WAVInfo
	Packets     []ingest.Packet // 注入故障后的实际投递序列
	ExpectFrames int64
	// RiffOverflow 在独立样例字节中注入 RIFF 长度溢出。
	RiffOverflow []byte
}

// Fixture 是一次任务生成的整套多轨夹具。
type Fixture struct {
	Tracks []*TrackFixture
	// CycleEdges 为依赖成环样例的边（独立于正常轨道图）。
	CycleEdges [][2]int
}

var crcIEEE = crc32.MakeTable(crc32.IEEE)

// BuildFixture 依据配置确定性地生成多轨 WAV 与包记录，并注入指定故障。
func BuildFixture(cfg Config) (*Fixture, error) {
	rng := rand.New(rand.NewSource(cfg.Seed))
	fix := &Fixture{}
	framesPerPacket := int64(256)

	for tid := 0; tid < cfg.Tracks; tid++ {
		tf := &TrackFixture{ID: tid, ExpectFrames: cfg.Samples}
		// 1) 生成确定性 PCM 并封装为合法 WAV。
		pcm := genPCM(rng, cfg.Samples)
		wavBytes := buildWAV(pcm, sampleRate, 1, 16)
		tf.WAV, tf.WAVSize = wavBytes, int64(len(wavBytes))
		info, err := ingest.ParseWAV(bytes.NewReader(wavBytes), int64(len(wavBytes)))
		if err != nil {
			return nil, fmt.Errorf("轨道 %d 自检 WAV 失败: %w", tid, err)
		}
		tf.Info = info

		// 2) 切分为带 CRC 与锚点的包记录。
		pkts := splitPackets(tid, pcm, framesPerPacket, cfg.NoAnchors)
		// 3) 注入弱网与内容故障。
		pkts = injectFaults(cfg, tid, pkts, rng)
		// 4) 回放持续变化的带宽计划：低带宽档积压溢出缓存产生突发丢失，
		//    高带宽档排空队列产生先丢后到的乱序形态。确定性、无网络依赖。
		pkts = shapeByBandwidth(cfg, pkts, framesPerPacket)
		tf.Packets = pkts
		fix.Tracks = append(fix.Tracks, tf)

		// 5) 独立的 RIFF 长度溢出样例（不进入正常母版流）。
		if cfg.Faults.RiffOverflow {
			bad := append([]byte(nil), wavBytes...)
			binary.LittleEndian.PutUint32(bad[4:], 0xFFFFFFFF)
			tf.RiffOverflow = bad
		}
	}

	// 6) 依赖成环样例：构造 A->B->A 的环，供 graph 阶段单独核验拒绝。
	if cfg.Faults.Cycle && cfg.Tracks >= 2 {
		fix.CycleEdges = [][2]int{{0, 1}, {1, 0}}
	}
	return fix, nil
}

// shapeByBandwidth 依据配置的带宽计划整形包投递顺序与丢失。
// 包等效比特数按 16bit 单声道帧载荷估算；时间片取 100ms。
func shapeByBandwidth(cfg Config, pkts []ingest.Packet, fpp int64) []ingest.Packet {
	if len(cfg.Bandwidth) == 0 {
		return pkts
	}
	const sliceMs int64 = 100
	packetBits := fpp * 2 * 8 // 每包帧数 * 2 字节 * 8 比特。
	plan, err := ingest.NewBandwidthPlan(cfg.Bandwidth, sliceMs, packetBits)
	if err != nil {
		return pkts // 参数已在上游校验，这里防御性回退到原始序列。
	}
	// 链路缓存容量与流水线乱序窗口对齐；每片注入量取窗口一半，
	// 使低带宽档（放行能力 < 注入）稳定产生积压与突发丢失。
	queueCap := cfg.reorderWindow()
	inject := queueCap / 2
	if inject < 1 {
		inject = 1
	}
	return plan.SimulateDelivery(pkts, queueCap, inject)
}

// TrackSampleCounts 返回各轨道期望采样总数，供检查点一致性核验。
func (f *Fixture) TrackSampleCounts() map[int]int64 {
	out := make(map[int]int64, len(f.Tracks))
	for _, tf := range f.Tracks {
		out[tf.ID] = tf.ExpectFrames
	}
	return out
}

// genPCM 生成确定性的 16bit 单声道 PCM 字节。
func genPCM(rng *rand.Rand, frames int64) []byte {
	pcm := make([]byte, frames*2)
	for i := int64(0); i < frames; i++ {
		v := int16(rng.Intn(60000) - 30000)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	return pcm
}

// buildWAV 手工拼装最小合法 PCM WAV（RIFF/fmt/data）。
func buildWAV(pcm []byte, rate uint32, chans, bits uint16) []byte {
	var b bytes.Buffer
	blockAlign := chans * bits / 8
	b.Write(riff.RiffID[:])
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.Write(riff.WavFormatID[:])
	b.Write(riff.FmtID[:])
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, ingest.WaveFormatPCM)
	binary.Write(&b, binary.LittleEndian, chans)
	binary.Write(&b, binary.LittleEndian, rate)
	binary.Write(&b, binary.LittleEndian, rate*uint32(blockAlign))
	binary.Write(&b, binary.LittleEndian, blockAlign)
	binary.Write(&b, binary.LittleEndian, bits)
	b.Write(riff.DataFormatID[:])
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// splitPackets 将 PCM 切成定长包并附带 CRC；默认每 4 包放置一个同步锚点，
// noAnchors 为 true 时完全不放锚点，用于演示关键帧缺标 N/A。
func splitPackets(tid int, pcm []byte, fpp int64, noAnchors bool) []ingest.Packet {
	total := int64(len(pcm) / 2)
	pkts := make([]ingest.Packet, 0, (total+fpp-1)/fpp)
	for seq, start := int64(0), int64(0); start < total; seq, start = seq+1, start+fpp {
		end := start + fpp
		if end > total {
			end = total
		}
		payload := pcm[start*2 : end*2]
		pkts = append(pkts, ingest.Packet{
			TrackID:     tid,
			Seq:         seq,
			StartSample: start,
			Frames:      end - start,
			Anchor:      !noAnchors && seq%4 == 0,
			Payload:     append([]byte(nil), payload...),
			CRC:         crc32.Checksum(payload, crcIEEE),
		})
	}
	return pkts
}

// injectFaults 按掩码注入重复、乱序、突发丢失与 CRC 故障，保持确定性。
func injectFaults(cfg Config, tid int, pkts []ingest.Packet, rng *rand.Rand) []ingest.Packet {
	out := append([]ingest.Packet(nil), pkts...)

	// CRC 错误：破坏中间一个包的载荷但保留旧 CRC。
	if cfg.Faults.CRCError && len(out) > 4 {
		idx := len(out) / 2
		out[idx].Payload[0] ^= 0xFF
	}
	// 尾块截断：最后一个包帧数缩短（模拟截断尾块）。
	if cfg.Faults.Truncation && len(out) > 2 {
		last := &out[len(out)-1]
		if last.Frames > 2 {
			last.Frames -= 2
			last.Payload = last.Payload[:last.Frames*2]
		}
	}
	// 重复包：复制一个完全相同的包插入序列。
	if cfg.Faults.Duplicate && len(out) > 3 {
		dup := out[1]
		out = append(out[:2], append([]ingest.Packet{dup}, out[2:]...)...)
	}
	// 乱序：交换两个相邻包位置。
	if cfg.Faults.Reorder && len(out) > 5 {
		i, j := 2, 3
		out[i], out[j] = out[j], out[i]
	}
	// 突发丢失：删除连续两个包（弱网突发丢包）。
	if cfg.Faults.BurstLoss && len(out) > 8 {
		drop := 5
		out = append(out[:drop], out[drop+2:]...)
	}
	_ = rng
	return out
}
