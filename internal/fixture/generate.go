// Package fixture 生成仓库内自包含的确定性帧样本。
// 全部输出由输入参数与序号决定，不读取任何外部数据源、不使用随机源，
// 因此相同参数重复生成的字节级结果完全一致。
package fixture

import (
	"crypto/sha256"
	"encoding/binary"

	"archivecheck/internal/model"
)

// Params 是样本生成的全部输入参数。
type Params struct {
	Frames       int      // 总帧数
	Reps         []string // 表示层，按切片顺序生成
	DriftPPM     int      // 采集时钟相对真实时间的漂移（百万分率）
	WrapAt       uint64   // RTP 时间戳回绕模
	CorruptFrame int      // 注入单帧哈希损坏的帧号，-1 表示不注入
	KeyInterval  int      // 关键帧间隔
	AudioRate    int64    // 音频采样率
	RTPClock     int64    // RTP 时钟频率（90kHz）
	VFR          bool     // 是否启用可变帧率
}

// DefaultParams 返回演示验收使用的默认参数（不含故障注入项）。
func DefaultParams() Params {
	return Params{
		Frames:       12000,
		Reps:         []string{"360p", "720p"},
		DriftPPM:     180,
		WrapAt:       4294960000,
		CorruptFrame: 8111,
		KeyInterval:  60,
		AudioRate:    48000,
		RTPClock:     90000,
		VFR:          true,
	}
}

// frameDurationNS 返回第 i 帧相对前一帧的真实时长（整数纳秒）。
// VFR 采用 4 帧周期且四档之和严格等于 4 倍基准；周期长度 4 整除关键帧
// 间隔 60，因此每个 60 帧分片都含整数个完整周期，净时长精确为 2s，
// 不会把周期内的帧间抖动错误累积成分片时长偏差。
func frameDurationNS(i int, vfr bool) int64 {
	const base = int64(33_333_333) // 1e9/30
	if !vfr {
		return base
	}
	step := base / 50
	switch i % 4 {
	case 0:
		return base - step
	case 1:
		return base + step
	case 2:
		return base - step
	default:
		return base + step
	}
}

// payload 生成指定表示层与帧号的确定性有效载荷。
func payload(rep string, seq uint32) []byte {
	h := sha256.New()
	h.Write([]byte(rep))
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], seq)
	h.Write(b[:])
	sum := h.Sum(nil)
	// 32 字节摘要扩展为 64 字节载荷，仍完全确定。
	return append(sum, sum...)
}

// Generate 按参数为每个表示层生成完整帧序列。
// TruthNS 仅用于评估侧比对，恢复管线禁止依赖该字段。
func Generate(p Params) map[string][]model.Frame {
	if p.WrapAt == 0 {
		p.WrapAt = 1 << 32
	}
	if p.KeyInterval <= 0 {
		p.KeyInterval = 60
	}
	if p.AudioRate <= 0 {
		p.AudioRate = 48000
	}
	if p.RTPClock <= 0 {
		p.RTPClock = 90000
	}
	out := make(map[string][]model.Frame, len(p.Reps))
	// 起始偏移贴近回绕点，使默认演示场景在开头数秒内真实触发一次回绕，
	// 同时不改变相邻帧的真实时间间隔。
	startOffset := int64(p.WrapAt) - p.RTPClock*5
	for _, rep := range p.Reps {
		frames := make([]model.Frame, 0, p.Frames)
		var truthNS int64
		for i := 0; i < p.Frames; i++ {
			if i > 0 {
				truthNS += frameDurationNS(i, p.VFR)
			}
			// 设备时钟带漂移：tick 相对真实时间多走 ppm 比例。
			tick := startOffset + int64(float64(truthNS)*float64(p.RTPClock)/1e9*
				(1+float64(p.DriftPPM)/1_000_000))
			raw := uint32(uint64(tick) % p.WrapAt)
			audio := uint32((float64(truthNS)/1e9*float64(p.AudioRate) + 0.5))
			pl := payload(rep, uint32(i))
			sum := sha256.Sum256(pl)
			f := model.Frame{
				Rep:          rep,
				Seq:          uint32(i),
				IsKey:        i%p.KeyInterval == 0,
				RTPSeq:       uint16(i),
				RTPTimestamp: raw,
				AudioSamples: audio,
				TruthNS:      truthNS,
				PayloadHash:  sum[:],
				Payload:      pl,
			}
			if i == p.CorruptFrame {
				f.Payload = append([]byte(nil), pl...)
				f.Payload[0] ^= 0xFF // 翻转首字节但保留原哈希，制造校验不符
			}
			frames = append(frames, f)
		}
		out[rep] = frames
	}
	return out
}

// InputDigest 对生成参数做确定性摘要，作为检查点续跑的输入指纹。
func InputDigest(p Params) [32]byte {
	h := sha256.New()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(p.Frames))
	h.Write(b[:])
	binary.BigEndian.PutUint64(b[:], p.WrapAt)
	h.Write(b[:])
	binary.BigEndian.PutUint64(b[:], uint64(p.DriftPPM))
	h.Write(b[:])
	binary.BigEndian.PutUint64(b[:], uint64(p.CorruptFrame))
	h.Write(b[:])
	for _, rep := range p.Reps {
		h.Write([]byte(rep))
		h.Write([]byte{0})
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}
