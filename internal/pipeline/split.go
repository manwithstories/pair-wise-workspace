package pipeline

import (
	"fmt"
	"sort"

	"fieldcut/internal/parse"
	"fieldcut/internal/timeline"
)

// CutConfig 控制切片行为。
type CutConfig struct {
	TargetNs      int64 // 目标片段时长
	MinNs         int64 // 短于此时长不单独成片
	SlopNs        int64 // 边界对齐允许的最大误差
}

// DefaultCutConfig 返回默认切分配置（约 1s 目标，0.25s 最小）。
func DefaultCutConfig() CutConfig {
	return CutConfig{TargetNs: 1_000_000_000, MinNs: 250_000_000,
		SlopNs: 1_500_000_000}
}

// Segmenter 依据视频随机访问点切分，并把音频/事件映射进同一时间线。
type Segmenter struct {
	cfg     CutConfig
	boundary map[int64]bool // 确认的随机访问点纳秒时间
}

// NewSegmenter 预计算视频轨安全切点。
func NewSegmenter(cfg CutConfig, video *parse.TrackInfo) *Segmenter {
	s := &Segmenter{cfg: cfg, boundary: make(map[int64]bool)}
	if video == nil {
		return s
	}
	for _, sm := range video.Samples {
		if sm.SyncKnown && sm.Sync {
			s.boundary[sm.TimeNs] = true
		}
	}
	return s
}

// nextSync 返回不早于 target 的最近安全切点及其误差（cut-target）；
// 没有任何确认随机访问点时 ok=false（调用方须把边界标为不安全）。
func (s *Segmenter) nextSync(target int64) (int64, int64, bool) {
	if len(s.boundary) == 0 {
		return 0, 0, false
	}
	times := make([]int64, 0, len(s.boundary))
	for t := range s.boundary {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	// 二分第一个 >= target。
	idx := sort.Search(len(times), func(i int) bool { return times[i] >= target })
	if idx >= len(times) {
		return 0, 0, false
	}
	return times[idx], times[idx] - target, true
}

// Split 对单个机位的视频轨执行切分，返回对齐安全切点的片段骨架。
// 无法确认随机访问点时 SafeCut=false，且不伪造切点（整轨退化为单段并降级）。
func (s *Segmenter) Split(ti *parse.TrackInfo) []*Segment {
	if len(ti.Samples) == 0 {
		return nil
	}
	start := ti.Samples[0].TimeNs
	var end int64
	for _, sm := range ti.Samples {
		if e := sm.TimeNs + sm.DurationNs; e > end {
			end = e
		}
	}
	if len(s.boundary) == 0 {
		return []*Segment{s.degradedSegment(ti, start, end)}
	}
	var segs []*Segment
	cur := start
	for cur < end {
		target := cur + s.cfg.TargetNs
		cut, errNs, ok := s.nextSync(target)
		if !ok || cut <= cur {
			// 目标之后再无安全切点，延伸到结尾；cur 本身已是安全切点。
			segs = append(segs, s.buildSegment(ti, cur, end, cur, true))
			break
		}
		safe := errNs <= s.cfg.SlopNs || cut == cur
		segs = append(segs, s.buildSegment(ti, cur, cut, cut, safe))
		cur = cut
	}
	return s.compact(segs)
}

func (s *Segmenter) buildSegment(ti *parse.TrackInfo, start, end,
	cutAt int64, safe bool) *Segment {
	var first, last uint32
	var ranges []ByteRange
	for _, sm := range ti.Samples {
		if sm.TimeNs < start || sm.TimeNs >= end {
			continue
		}
		if first == 0 {
			first = sm.Index
		}
		last = sm.Index
		if sm.Offset > 0 && sm.Size > 0 {
			ranges = append(ranges, ByteRange{
				Offset: int64(sm.Offset), Length: int64(sm.Size)})
		}
	}
	return &Segment{
		Camera: cameraLabel(ti), TrackID: ti.TrackID,
		StartNs: start, EndNs: end, ErrNs: end - cutAt,
		SafeCut: safe, StartSample: first, EndSample: last,
		ByteRanges: ranges, Degraded: !safe || ti.IndexGap,
	}
}

func (s *Segmenter) degradedSegment(ti *parse.TrackInfo, start, end int64) *Segment {
	seg := s.buildSegment(ti, start, end, start, false)
	seg.Degraded = true
	return seg
}

// compact 合并短于 MinNs 的尾部片段到前一段。
func (s *Segmenter) compact(segs []*Segment) []*Segment {
	if len(segs) <= 1 {
		return segs
	}
	out := []*Segment{segs[0]}
	for _, seg := range segs[1:] {
		prev := out[len(out)-1]
		if seg.EndNs-seg.StartNs < s.cfg.MinNs {
			prev.EndNs = seg.EndNs
			prev.EndSample = seg.EndSample
			prev.ByteRanges = append(prev.ByteRanges, seg.ByteRanges...)
			prev.Degraded = prev.Degraded || seg.Degraded
			continue
		}
		out = append(out, seg)
	}
	return out
}

// AttachEvents 把跨轨事件按起止时间裁切/归属到片段，输出来源轨与误差。
func AttachEvents(segs []*Segment, events []parse.Event) []AssignedEvent {
	var assigned []AssignedEvent
	for _, seg := range segs {
		for _, ev := range events {
			if ev.EndNs <= seg.StartNs || ev.StartNs >= seg.EndNs {
				continue
			}
			cp := ev
			// 跨边界事件裁到片段范围。
			if cp.StartNs < seg.StartNs {
				cp.StartNs = seg.StartNs
			}
			if cp.EndNs > seg.EndNs {
				cp.EndNs = seg.EndNs
			}
			assigned = append(assigned, AssignedEvent{
				SegmentID: seg.ID, Event: cp, SourceTrack: ev.TrackID,
			})
		}
	}
	return assigned
}

// AssignedEvent 绑定片段与裁切后的事件。
type AssignedEvent struct {
	SegmentID   string
	SourceTrack uint32
	Event       parse.Event
}

// MapAudio 把音频轨样本按时间窗归属到视频片段，返回每段覆盖的样本序号。
func MapAudio(segs []*Segment, audio *parse.TrackInfo) map[string][][2]uint32 {
	out := make(map[string][][2]uint32)
	for _, seg := range segs {
		var lo, hi uint32
		for _, sm := range audio.Samples {
			if sm.TimeNs >= seg.StartNs && sm.TimeNs < seg.EndNs {
				if lo == 0 {
					lo = sm.Index
				}
				hi = sm.Index
			}
		}
		if lo != 0 {
			out[seg.ID] = append(out[seg.ID], [2]uint32{lo, hi})
		}
	}
	return out
}

func cameraLabel(ti *parse.TrackInfo) string {
	return fmt.Sprintf("cam-%d", ti.TrackID)
}

// 保证 timeline 包在未来扩展中保持引用（依赖图排序由上层编排使用）。
var _ = timeline.KindVideo
