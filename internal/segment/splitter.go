// Package segment 按统一采样时间线把连续 PCM 块切成节目分段。
// 切分严格落在采样帧边界上，静音边界可选，重叠边界不重复计入时长。
package segment

import (
	"sort"

	"media-pipeline/internal/media"
)

// SilenceCut 是静音检测给出的候选切点（静音区间的中点）。
type SilenceCut struct {
	Sample uint64 // 候选切点采样序号
}

// Segment 是一个完整节目分段，覆盖 [FirstSample, EndSample)。
type Segment struct {
	Index       uint64
	FirstSample uint64
	EndSample   uint64
	Frames      uint64
	PCM         []byte
	Gaps        []media.Gap // 落在本分段时间线内的缺口证据
}

// Config 配置切分策略。
type Config struct {
	TargetFrames uint64 // 目标分段帧数（时长）
	MinFrames    uint64 // 末段最小帧数，不足则并入上一段
}

// Splitter 跨块累积 PCM，保证窗口跨块时切分结果与整段输入完全一致。
type Splitter struct {
	cfg         Config
	frameSize   int
	baseSample  uint64 // buf 首帧的全局采样序号
	buf         []byte
	gaps        []media.Gap
	nextIndex   uint64
	silenceCuts []uint64 // 静音候选切点，单调排序
}

// New 构造切分器，frameSize 为时间基准帧大小。
func New(cfg Config, frameSize int) *Splitter {
	return &Splitter{cfg: cfg, frameSize: frameSize}
}

// AddSilenceBoundary 注册一个静音切点；只接受单调前进且不与既有切点重合者。
func (s *Splitter) AddSilenceBoundary(cut SilenceCut) {
	if len(s.silenceCuts) > 0 && cut.Sample <= s.silenceCuts[len(s.silenceCuts)-1] {
		return
	}
	s.silenceCuts = append(s.silenceCuts, cut.Sample)
}

// Push 追加一个连续 PCM 块并返回本次可切出的完整分段。
func (s *Splitter) Push(b media.Block) ([]Segment, error) {
	if len(s.buf) == 0 {
		s.baseSample = b.FirstSample
	}
	s.buf = append(s.buf, b.PCM...)
	s.gaps = append(s.gaps, b.Gaps...)
	sort.Slice(s.gaps, func(i, j int) bool { return s.gaps[i].FirstSample < s.gaps[j].FirstSample })
	return s.emit(false), nil
}

// nextBoundary 返回下一个切点的全局采样序号；优先静音边界，其次目标时长。
func (s *Splitter) nextBoundary() (uint64, bool) {
	target := s.baseSample + s.cfg.TargetFrames
	for len(s.silenceCuts) > 0 {
		c := s.silenceCuts[0]
		if c <= s.baseSample {
			s.silenceCuts = s.silenceCuts[1:] // 已落后于当前段起点，丢弃
			continue
		}
		if c < target {
			s.silenceCuts = s.silenceCuts[1:]
			return c, true
		}
		break
	}
	return target, s.bufferedFrames() >= s.cfg.TargetFrames
}

func (s *Splitter) bufferedFrames() uint64 {
	return uint64(len(s.buf) / s.frameSize)
}

// Flush 输出剩余 PCM 作为末段（不足 MinFrames 则并入上一段由调用方处理，
// 这里仍返回但标记帧数）。
func (s *Splitter) Flush() []Segment {
	out := s.emit(true)
	if len(s.buf) > 0 {
		seg := s.cut(s.baseSample+s.bufferedFrames())
		out = append(out, seg)
	}
	return out
}

func (s *Splitter) emit(final bool) []Segment {
	var out []Segment
	for {
		cut, ready := s.nextBoundary()
		if !ready {
			return out
		}
		bufferedEnd := s.baseSample + s.bufferedFrames()
		if !final && cut >= bufferedEnd {
			return out // 切点尚未跨进已缓冲数据
		}
		if cut > bufferedEnd {
			cut = bufferedEnd
		}
		out = append(out, s.cut(cut))
	}
}

// cut 在全局采样序号 at 处落刀，切出 [baseSample, at) 并推进缓冲。
// 缺口按 FirstSample 归属分段，任何采样帧只可能属于一个分段。
func (s *Splitter) cut(at uint64) Segment {
	frames := at - s.baseSample
	byteLen := int(frames) * s.frameSize
	pcm := make([]byte, byteLen)
	copy(pcm, s.buf[:byteLen])

	var segGaps []media.Gap
	keep := s.gaps[:0]
	for _, g := range s.gaps {
		if g.FirstSample < at {
			segGaps = append(segGaps, g)
		} else {
			keep = append(keep, g)
		}
	}
	s.gaps = append([]media.Gap(nil), keep...)

	seg := Segment{
		Index:       s.nextIndex,
		FirstSample: s.baseSample,
		EndSample:   at,
		Frames:      frames,
		PCM:         pcm,
		Gaps:        segGaps,
	}
	s.nextIndex++
	s.buf = append([]byte(nil), s.buf[byteLen:]...)
	s.baseSample = at
	return seg
}

// BufferedFrames 暴露当前滞留帧数，供测试与背压监控。
func (s *Splitter) BufferedFrames() uint64 { return s.bufferedFrames() }
