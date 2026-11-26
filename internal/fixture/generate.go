// Package fixture 在用户指定目录内自生成全部演示样例，不引用任何外部文件。
//
// 样例清单：
//   - normal.arc      正常双轨素材（视频+音频，含静音段）
//   - truncated.arc   损坏尾包，前缀可恢复
//   - wrap.arc        32 位时间戳回绕
//   - illegal.arc     媒体正常，但剪辑计划含越界剪辑点
//   - transition.arc  双轨重叠，剪辑计划含重复转场声明
//
// 所有负载确定性生成，保证重复运行字节级一致。
package fixture

import (
	"os"
	"path/filepath"

	"archivemedia/internal/adapter"
	"archivemedia/internal/model"
)

// 源类型标识。
const (
	KindNormal     = "normal"
	KindTruncated  = "truncated"
	KindWrap       = "wrap"
	KindIllegalCut = "illegal_cut"
	KindTransition = "transition"
	KindRealMKV    = "real_mkv"
)

// CutPlan 是某个源的剪辑计划（统一轴微秒）。
type CutPlan struct {
	PlanRanges  []PlanRange
	Transitions []PlanTransition
}

// PlanRange 是一条剪辑请求。
type PlanRange struct {
	TrackIndex     int
	StartUs, EndUs int64
	Mode           model.Rounding
	ExpectInvalid  bool // 非法剪辑点样例：预期被切分引擎拒绝
}

// PlanTransition 是转场声明。
type PlanTransition struct {
	TrackA, TrackB int
	StartUs, EndUs int64
	Repeat         int // 重复声明次数（去重演示）
}

// Source 是一个生成好的样例及其处理计划。
type Source struct {
	Kind             string
	Path             string
	TargetBitrateBps int64
	Plan             CutPlan
	// Parse 选项覆盖（回绕样例需要关闭时长上限）。
	NoDurationLimit bool
	ExpectCorrupt   bool
}

const (
	videoTB      = 25   // 25fps
	audioSR      = 8000 // 8kHz
	framePayload = 4096
)

// videoTrackSpec / audioTrackSpec 构造标准轨道。
func videoTrackSpec(wrapBits int) adapter.ARCTrackSpec {
	return adapter.ARCTrackSpec{
		Type: adapter.MediaVideo, Codec: "arc-test-video",
		TimeBase: model.MustRational(1, videoTB), Width: 64, Height: 48,
		WrapBits: wrapBits,
	}
}

func audioTrackSpec() adapter.ARCTrackSpec {
	return adapter.ARCTrackSpec{
		Type: adapter.MediaAudio, Codec: "arc-pcm-s16",
		TimeBase: model.MustRational(1, audioSR), SampleRate: audioSR, Channels: 1,
	}
}

// gradientPayload 确定性视频负载：随帧号变化，重复帧内容相同（供指纹去重）。
func gradientPayload(frame int, duplicate bool) []byte {
	b := make([]byte, framePayload)
	if duplicate {
		// 重复素材：固定种子且不含帧号，任意帧号生成的内容完全一致。
		const seed = byte(9)
		for i := range b {
			b[i] = byte(int(seed)*31 + i*13)
		}
		return b
	}
	seed := byte(frame % 7)
	for i := range b {
		b[i] = byte(int(seed)*31 + i*13 + frame)
	}
	return b
}

// tonePayload 生成 s16le 单声道音频块；silent=true 时全零。
func tonePayload(samples int, silent bool, amp int16) []byte {
	b := make([]byte, samples*2)
	if silent {
		return b
	}
	for i := 0; i < samples; i++ {
		v := amp
		lo := byte(v)
		hi := byte(v >> 8)
		b[i*2] = lo
		b[i*2+1] = hi
	}
	return b
}

// GenerateAll 在 dir/sources 下生成全部 arc1 样例（mkv 由 GenerateMKV 单独生成）。
func GenerateAll(dir string) ([]Source, error) {
	srcDir := filepath.Join(dir, "sources")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return nil, model.Wrap(model.KindPathViolation, err, "fixture: mkdir %s", srcDir)
	}
	var out []Source

	if s, err := genNormal(srcDir); err != nil {
		return nil, err
	} else {
		out = append(out, s)
	}
	if s, err := genTruncated(srcDir); err != nil {
		return nil, err
	} else {
		out = append(out, s)
	}
	if s, err := genWrap(srcDir); err != nil {
		return nil, err
	} else {
		out = append(out, s)
	}
	if s, err := genIllegalCut(srcDir); err != nil {
		return nil, err
	} else {
		out = append(out, s)
	}
	if s, err := genTransition(srcDir); err != nil {
		return nil, err
	} else {
		out = append(out, s)
	}
	return out, nil
}

// frameUs 把视频帧号转成微秒。
func frameUs(frame int) int64 { return int64(frame) * model.MicrosPerSecond / videoTB }

// audioBlocks 生成 count 个 1000 采样（125ms）音频包，前 silentBlocks 块静音。
func audioPackets(startBlock, count, silentBlocks int) []adapter.ARCPacket {
	pkts := make([]adapter.ARCPacket, 0, count)
	for i := 0; i < count; i++ {
		silent := i < silentBlocks
		pkts = append(pkts, adapter.ARCPacket{
			TrackIndex:    1,
			StartTicks:    int64((startBlock + i) * 1000),
			DurationTicks: 1000,
			Payload:       tonePayload(1000, silent, 12000),
		})
	}
	return pkts
}
