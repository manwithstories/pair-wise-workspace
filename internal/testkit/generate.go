package testkit

import (
	"math/rand"

	"pairflow/internal/rtp"
)

// 固定注入位置：全部由常量决定，保证 seed 复现时结果可断言。
const (
	FramesPerBlock = 24
	BlockCount     = 40
	BurstLen       = 3 // 每台设备一次连续 3 包突发丢失
	BurstAtIndex   = 10
	DupIndex       = 5  // 重复包注入位置
	SwapIndex      = 20 // 相邻交换的乱序注入位置
	TruncIndex     = 30 // WAV 头截断块位置（dev-c）
	DriftIndex     = 15 // RTP 时间戳漂移块位置（dev-c）
)

// DeviceSpec 是单台采集设备的素材参数。
type DeviceSpec struct {
	DeviceID string
	SSRC     uint32
	Format   PCMFormat
	// StartSeq/StartTS 刻意靠近回绕边界以覆盖序号/时间戳回绕。
	StartSeq uint16
	StartTS  uint32
}

// BlockRef 定位一个被注入故障的块。
type BlockRef struct {
	DeviceID string `json:"device_id"`
	RTPSeq   uint64 `json:"rtp_seq"`
	Index    int    `json:"block_index"`
}

// Spec 是夹具的地面真值，供测试断言与 demo 摘要对账。
type Spec struct {
	Seed               int64       `json:"seed"`
	BandwidthTrace     string      `json:"bandwidth_trace"`
	Devices            []DeviceSpec `json:"devices"`
	DuplicatePackets   int         `json:"duplicate_packets"`
	ConflictPackets    int         `json:"conflict_packets"`
	ReorderSwaps       int         `json:"reorder_swaps"`
	BurstLossPerDevice int         `json:"burst_loss_packets_per_device"`
	LostBlocks         []BlockRef  `json:"lost_blocks"`
	TruncatedBlocks    []BlockRef  `json:"truncated_blocks"`
	DriftBlocks        []BlockRef  `json:"drift_blocks"`
	TSWrapDevices      []string    `json:"ts_wrap_devices"`
	SeqWrapDevices     []string    `json:"seq_wrap_devices"`
}

// DeviceCapture 是一台设备注入网络效应后的采集记录序列。
type DeviceCapture struct {
	Spec     DeviceSpec
	Records  []rtp.CaptureRecord
	LostSeqs []uint64
}

// Fixture 是完整的自生成素材集合。
type Fixture struct {
	Spec     Spec
	Captures []DeviceCapture
}

// DefaultDeviceSpecs 返回 3 台 48kHz/16bit/立体声设备的规格，
// 码率恰为 192000 bps，与验收的 target-bitrate 对齐。
func DefaultDeviceSpecs() []DeviceSpec {
	f := PCMFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
	// 让 dev-b 在素材中段越过 32bit 时间戳边界（变量运算，允许回绕语义）。
	var maxTS uint32 = ^uint32(0)
	devBStartTS := maxTS - uint32(TruncIndex*FramesPerBlock) + 1
	return []DeviceSpec{
		{DeviceID: "dev-a", SSRC: 0xA0000001, Format: f, StartSeq: 65530, StartTS: 1000},
		{DeviceID: "dev-b", SSRC: 0xB0000002, Format: f, StartSeq: 40000, StartTS: devBStartTS},
		{DeviceID: "dev-c", SSRC: 0xC0000003, Format: f, StartSeq: 20000, StartTS: 500000},
	}
}

type builtPkt struct {
	index   int
	seq     uint16
	ts      uint32
	payload []byte
}

// GenerateFixture 生成完整夹具；trace 为 "steady" 或 "changing"。
func GenerateFixture(seed int64, trace string) *Fixture {
	rng := rand.New(rand.NewSource(seed))
	specs := DefaultDeviceSpecs()
	fx := &Fixture{Spec: Spec{Seed: seed, BandwidthTrace: trace,
		BurstLossPerDevice: BurstLen, Devices: specs}}

	for di, ds := range specs {
		pkts := buildOrdered(ds, seed, di, &fx.Spec)
		pkts = injectBurstLoss(pkts, ds, &fx.Spec)
		pkts = injectDuplicates(pkts, ds, di, &fx.Spec)
		pkts = injectReorder(pkts, &fx.Spec)
		recs := assignArrivals(pkts, ds, rng, trace)
		fx.Captures = append(fx.Captures, DeviceCapture{
			Spec: ds, Records: recs, LostSeqs: lostSeqList(ds),
		})
	}
	return fx
}

func buildOrdered(ds DeviceSpec, seed int64, di int, spec *Spec) []builtPkt {
	out := make([]builtPkt, 0, BlockCount)
	tsBase := ds.StartTS
	for i := 0; i < BlockCount; i++ {
		seq := ds.StartSeq + uint16(i)
		ts := tsBase + uint32(i*FramesPerBlock)
		pcm := SynthesizePCM(ds.Format, i, FramesPerBlock, seed+int64(di))
		payload := BuildWAVBlock(ds.Format, pcm)
		// dev-c：一个 RTP 时间戳漂移块（delta 少 1 采样），交由采样计数修复。
		if ds.DeviceID == "dev-c" && i == DriftIndex {
			ts--
			spec.DriftBlocks = append(spec.DriftBlocks,
				BlockRef{DeviceID: ds.DeviceID, Index: i, RTPSeq: uint64(seq)})
		}
		// dev-c：一个 WAV 头截断块（仅保留 8 字节，RTP 头仍完好）。
		if ds.DeviceID == "dev-c" && i == TruncIndex {
			payload = TruncateWAV(payload, 8)
			spec.TruncatedBlocks = append(spec.TruncatedBlocks,
				BlockRef{DeviceID: ds.DeviceID, Index: i, RTPSeq: uint64(seq)})
		}
		out = append(out, builtPkt{index: i, seq: seq, ts: ts, payload: payload})
	}
	span := uint64(BlockCount) * uint64(FramesPerBlock)
	if uint64(ds.StartTS)+span > 1<<32 {
		spec.TSWrapDevices = append(spec.TSWrapDevices, ds.DeviceID)
	}
	if int(ds.StartSeq)+BlockCount > 65536 {
		spec.SeqWrapDevices = append(spec.SeqWrapDevices, ds.DeviceID)
	}
	return out
}

func injectBurstLoss(pkts []builtPkt, ds DeviceSpec, spec *Spec) []builtPkt {
	out := make([]builtPkt, 0, len(pkts)-BurstLen)
	for _, p := range pkts {
		if p.index >= BurstAtIndex && p.index < BurstAtIndex+BurstLen {
			spec.LostBlocks = append(spec.LostBlocks,
				BlockRef{DeviceID: ds.DeviceID, Index: p.index, RTPSeq: uint64(p.seq)})
			continue
		}
		out = append(out, p)
	}
	return out
}

func injectDuplicates(pkts []builtPkt, ds DeviceSpec, di int, spec *Spec) []builtPkt {
	if ds.DeviceID != "dev-a" && ds.DeviceID != "dev-b" {
		return pkts
	}
	out := make([]builtPkt, 0, len(pkts)+1)
	for _, p := range pkts {
		out = append(out, p)
		if p.index != DupIndex {
			continue
		}
		dup := p
		if ds.DeviceID == "dev-b" {
			dup.payload = MutatePayload(p.payload) // 内容冲突副本
			spec.ConflictPackets++
		}
		spec.DuplicatePackets++
		out = append(out, dup)
	}
	_ = di
	return out
}

func injectReorder(pkts []builtPkt, spec *Spec) []builtPkt {
	out := append([]builtPkt(nil), pkts...)
	for i := range out {
		if out[i].index == SwapIndex+1 {
			out[i], out[i-1] = out[i-1], out[i]
			spec.ReorderSwaps++
			break
		}
	}
	return out
}

func assignArrivals(pkts []builtPkt, ds DeviceSpec, rng *rand.Rand, trace string) []rtp.CaptureRecord {
	recs := make([]rtp.CaptureRecord, 0, len(pkts))
	now := int64(1_000_000_000)
	for i, p := range pkts {
		var step int64
		switch trace {
		case "changing":
			// 8 包快、8 包慢交替，模拟带宽持续变化下的块间隔抖动。
			if (i/8)%2 == 0 {
				step = 2_000_000
			} else {
				step = 20_000_000
			}
		default:
			step = 10_000_000
		}
		now += step + int64(rng.Intn(2_000_000))
		pkt := BuildRTPPacket(p.seq, p.ts, ds.SSRC, p.payload)
		raw, _ := pkt.Marshal()
		recs = append(recs, rtp.CaptureRecord{
			DeviceID: ds.DeviceID, ArrivalNano: now, Packet: raw,
		})
	}
	return recs
}

func lostSeqList(ds DeviceSpec) []uint64 {
	var out []uint64
	for i := BurstAtIndex; i < BurstAtIndex+BurstLen; i++ {
		out = append(out, uint64(ds.StartSeq+uint16(i)))
	}
	return out
}
