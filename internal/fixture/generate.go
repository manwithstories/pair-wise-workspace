// Package fixture 生成完全由程序构造的 MP4、RTP dump 与时间线 sidecar，
// 不引用任何外部素材。生成结果可复现：相同参数得到字节一致的输出。
package fixture

import (
	"fmt"
	"os"
	"path/filepath"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
)

// Pattern 决定夹具注入的异常类型。
type Pattern string

const (
	PatternClean      Pattern = "clean"      // 连续，无异常
	PatternClipJump   Pattern = "clipjump"   // MP4 剪辑边界出现大跳变
	PatternDrift      Pattern = "drift"      // MP4 时钟轻微漂移
	PatternWrap       Pattern = "wrap"       // RTP 时间戳 32 位回绕
	PatternDuplicate  Pattern = "duplicate"  // RTP 重复时间戳
	PatternRegression Pattern = "regression" // RTP 时间戳倒退
)

// Spec 描述一组夹具的规模。所有字段都有硬上限，超限直接拒绝。
type Spec struct {
	VideoSamples   int     // 视频样本数，[8, 4096]
	AudioSamples   int     // 音频样本数，[0, 4096]
	VideoTimescale uint32  // 默认 90_000
	AudioTimescale uint32  // 默认 48_000
	Pattern        Pattern // 注入的异常类型
}

// DefaultSpec 返回验收演示使用的默认规模。
func DefaultSpec() Spec {
	return Spec{VideoSamples: 120, AudioSamples: 60, VideoTimescale: 90_000, AudioTimescale: 48_000, Pattern: PatternClean}
}

// Validate 校验规模与上限。
func (s Spec) Validate() error {
	if s.VideoSamples < 8 || s.VideoSamples > 4096 {
		return fmt.Errorf("fixture: 视频样本数 %d 超出 [8,4096]", s.VideoSamples)
	}
	if s.AudioSamples < 0 || s.AudioSamples > 4096 {
		return fmt.Errorf("fixture: 音频样本数 %d 超出 [0,4096]", s.AudioSamples)
	}
	if s.VideoTimescale == 0 || s.VideoTimescale > model.MaxTimescale {
		return fmt.Errorf("fixture: 视频时钟频率 %d 非法", s.VideoTimescale)
	}
	if s.AudioTimescale == 0 || s.AudioTimescale > model.MaxTimescale {
		return fmt.Errorf("fixture: 音频时钟频率 %d 非法", s.AudioTimescale)
	}
	switch s.Pattern {
	case PatternClean, PatternClipJump, PatternDrift, PatternWrap, PatternDuplicate, PatternRegression:
		return nil
	}
	return fmt.Errorf("fixture: 未知时间戳模式 %q", s.Pattern)
}

// Files 是生成结果的路径集合。
type Files struct {
	Timeline      string            // 有效多轨时间线 MP4
	Corrupt       string            // 截断的 MP4（兼容旧名）
	Corrupts      map[string]string // 损坏种类 -> MP4 路径
	CorruptSides  map[string]string // 损坏种类 -> 对应 sidecar 路径
	TailTruncated string            // 箱头完整、样本越界的可恢复截断样例
	Sidecar       string            // 时间线 sidecar JSON
	RTPDump       string            // RTP 时间戳 dump
}

// Generate 在 dir 下生成全部夹具，返回路径集合。
func Generate(dir string, spec Spec) (Files, error) {
	if err := spec.Validate(); err != nil {
		return Files{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Files{}, fault.Wrap(fault.StageIO, 0, -1, false, "创建夹具目录失败", err)
	}
	tl, lay, err := buildLayout(spec)
	if err != nil {
		return Files{}, err
	}
	files := Files{
		Timeline: filepath.Join(dir, "timeline.mp4"),
		Corrupt:  filepath.Join(dir, "corrupt.mp4"),
		Sidecar:  filepath.Join(dir, "timeline.json"),
		RTPDump:  filepath.Join(dir, "rtp.dump"),
	}
	if err := writeMP4(files.Timeline, spec, lay); err != nil {
		return Files{}, err
	}
	if err := writeSidecar(files.Sidecar, tl); err != nil {
		return Files{}, err
	}
	if err := writeTruncated(files.Corrupt, files.Timeline); err != nil {
		return Files{}, err
	}
	files.TailTruncated = filepath.Join(dir, "corrupt_tail.mp4")
	if _, err := writeTailTruncated(files.TailTruncated, files.Timeline); err != nil {
		return Files{}, err
	}
	corrupts, err := writeCorruptSet(dir, files.Timeline, files.Sidecar)
	if err != nil {
		return Files{}, err
	}
	files.Corrupts = make(map[string]string, len(CorruptKinds))
	files.CorruptSides = make(map[string]string, len(CorruptKinds))
	for path, kind := range corrupts {
		if filepath.Ext(path) == ".json" {
			files.CorruptSides[kind] = path
		} else {
			files.Corrupts[kind] = path
		}
	}
	if err := writeRTPDump(files.RTPDump, spec); err != nil {
		return Files{}, err
	}
	return files, nil
}

// maxFixtureBytes 限制单个夹具的样本负载总量，防止误用大参数耗尽磁盘。
const maxFixtureBytes = 8 << 20
