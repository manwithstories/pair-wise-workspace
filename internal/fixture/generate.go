// Package fixture 生成自包含的 mini-WebM 样例与五套场景配置，
// 不引用任何外部文件；同种子产出逐字节一致。
package fixture

import (
	"bytes"

	"bcastqc/internal/media"
	"bcastqc/internal/mux"
	"bcastqc/internal/netem"
)

// 场景套件名。
const (
	SuiteClean     = "clean"
	SuiteRecovery  = "recovery"
	SuiteTruncate  = "truncate"
	SuiteOverflow  = "overflow"
	SuiteStall     = "stall"
)

// AllSuites 为演示默认覆盖的全部场景。
var AllSuites = []string{
	SuiteClean, SuiteRecovery, SuiteTruncate, SuiteOverflow, SuiteStall,
}

// Spec 描述一个待处理样例及其链路与队列参数。
type Spec struct {
	Name        string
	Seed        int64
	QueueBytes  int64
	CacheBytes  int64
	Window      int64
	VideoFrames int
	AudioBlocks int
	Link        netem.Config
	// TruncateBytes>0 时在生成文件尾部截断该量，制造结构损坏。
	TruncateBytes int64
	// ExpectReject 为 true 表示该样例预期被拒绝、不产出文件。
	ExpectReject bool
}

// 固定的内容规模，保证演示在毫秒级完成且统计稳定。
const (
	videoFrames = 120
	audioBlocks = 360
	payloadSize = 240
)

// Generate 依据规格生成一份 mini-WebM 字节流（可能被截断）。
func Generate(s Spec) ([]byte, error) {
	tracks := []mux.TrackSpec{
		{Number: media.TrackAudio, Type: 1 /* video */ + 1},
		{Number: media.TrackVideo, Type: 1},
	}
	blocks := buildBlocks(s)
	out, err := mux.Write(tracks, blocks)
	if err != nil {
		return nil, err
	}
	if s.TruncateBytes > 0 && int64(len(out)) > s.TruncateBytes {
		out = out[:int64(len(out))-s.TruncateBytes]
	}
	return out, nil
}

// buildBlocks 交错生成音视频块，时间线严格单调（30fps 视频 / 10ms 音频）。
func buildBlocks(s Spec) []mux.MuxBlock {
	var blocks []mux.MuxBlock
	vf := s.VideoFrames
	if vf <= 0 {
		vf = videoFrames
	}
	ab := s.AudioBlocks
	if ab <= 0 {
		ab = audioBlocks
	}
	const frameNS int64 = media.NSPerSecond / 30
	const audioNS int64 = media.NSPerSecond / 100
	vi, ai := 0, 0
	nextV, nextA := int64(0), int64(0)
	for vi < vf || ai < ab {
		if ai < ab && (vi >= vf || nextA <= nextV) {
			blocks = append(blocks, mux.MuxBlock{
				Track:   media.TrackAudio,
				TimeNS:  nextA,
				Payload: deterministicPayload(media.TrackAudio, ai),
			})
			ai++
			nextA += audioNS
		} else if vi < vf {
			blocks = append(blocks, mux.MuxBlock{
				Track:   media.TrackVideo,
				TimeNS:  nextV,
				Key:     vi%10 == 0,
				Payload: deterministicPayload(media.TrackVideo, vi),
			})
			vi++
			nextV += frameNS
		}
	}
	return blocks
}

// deterministicPayload 产出仅依赖轨道与序号的确定性负载。
func deterministicPayload(track uint32, idx int) []byte {
	b := make([]byte, payloadSize)
	seed := uint32(track)*1_000_003 + uint32(idx)*97
	for i := range b {
		seed = seed*1103515245 + 12345
		b[i] = byte(seed >> 16)
	}
	return b
}

// SpecFor 依据套件名与公共参数构建场景规格。
func SpecFor(suite string, seed int64, queueBytes int64) Spec {
	base := Spec{
		Name:       suite,
		Seed:       seed,
		QueueBytes: queueBytes,
		CacheBytes: queueBytes / 2,
		Window:     32,
		Link:       netem.Config{Seed: seed, OneWayDelayNS: 2_000_000},
	}
	switch suite {
	case SuiteClean:
		// 无损伤。
	case SuiteRecovery:
		base.Link.LossGood = 0.06
		base.Link.LossBad = 0.8
		base.Link.RecoverBad = 0.35
		base.Link.Duplicate = 0.02
		base.Link.Reorder = 0.03
		base.Window = 64
	case SuiteTruncate:
		base.TruncateBytes = 60
		base.ExpectReject = true
	case SuiteOverflow:
		// 小队列 + 高重复注入，验证字节硬上限与背压。
		base.QueueBytes = 32 * 1024
		base.CacheBytes = 16 * 1024
		base.Link.Duplicate = 0.25
		base.Link.BaseBPS = 256 * 1024
		base.Link.BandJitter = 0.4
	case SuiteStall:
		base.Link.StallStartNS = 400_000_000
		base.Link.StallEndNS = 900_000_000
		base.Link.BaseBPS = 512 * 1024
	}
	return base
}

// PayloadEqual 供测试比对确定性负载。
func PayloadEqual(a, b []byte) bool { return bytes.Equal(a, b) }
