package segment

import (
	"errors"
	"fmt"
	"sort"

	"tsproof/internal/clock"
)

// ErrSeek 表示定位点无法对齐到关键帧。
var ErrSeek = errors.New("segment: seek target unreachable")

// KeyframeLocator 建立视频轨道关键帧索引，并核对随机定位命中率。
type KeyframeLocator struct {
	// 关键帧在统一时间轴上的升序时刻。
	times []clock.Tick
	// 与 times 平行的轨道事件下标。
	eventIndex []int
}

// BuildKeyframeLocator 从时间线视频轨道提取关键帧索引。
func BuildKeyframeLocator(tr *clock.TrackTimeline) *KeyframeLocator {
	idx := tr.Keyframes()
	loc := &KeyframeLocator{
		times:      make([]clock.Tick, 0, len(idx)),
		eventIndex: make([]int, 0, len(idx)),
	}
	for _, ei := range idx {
		loc.times = append(loc.times, tr.Events[ei].At)
		loc.eventIndex = append(loc.eventIndex, ei)
	}
	return loc
}

// Size 返回关键帧总数。
func (l *KeyframeLocator) Size() int { return len(l.times) }

// Seek 返回不晚于 target 的最近关键帧时刻与事件下标。
// target 早于首个关键帧时返回 ErrSeek。
func (l *KeyframeLocator) Seek(target clock.Tick) (at clock.Tick, eventIndex int, err error) {
	if len(l.times) == 0 || target < l.times[0] {
		return 0, 0, fmt.Errorf("%w: target=%d", ErrSeek, target)
	}
	// sort.Search 找到第一个 times[i] > target，回退一格即最近关键帧。
	i := sort.Search(len(l.times), func(i int) bool { return l.times[i] > target })
	i--
	return l.times[i], l.eventIndex[i], nil
}

// SeekReport 是一次随机定位核对的结果。
type SeekReport struct {
	Requests int
	Hits     int
	Misses   int
}

// HitRate 以百分数返回命中率。
func (s SeekReport) HitRate() float64 {
	if s.Requests == 0 {
		return 0
	}
	return float64(s.Hits) * 100 / float64(s.Requests)
}

// VerifySeeds 用生成器已知的关键帧时刻作为定位请求，核对每个请求都能
// 精确命中（返回时刻等于已知关键帧时刻）。这是验收 keyframe_hit 的依据。
func (l *KeyframeLocator) VerifySeeds(knownKeyPTS []clock.Tick) SeekReport {
	rep := SeekReport{Requests: len(knownKeyPTS)}
	known := map[clock.Tick]bool{}
	for _, t := range l.times {
		known[t] = true
	}
	for _, want := range knownKeyPTS {
		at, _, err := l.Seek(want)
		if err == nil && at == want && known[want] {
			rep.Hits++
		} else {
			rep.Misses++
		}
	}
	return rep
}
