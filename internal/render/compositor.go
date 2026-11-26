package render

import (
	"fmt"
	"image"
	"sort"

	"aircheck/internal/media"

	"github.com/disintegration/imaging"
)

// maxCacheEntries 源图像缓存上限（有界）。
const maxCacheEntries = 32

// TrackSlots 一条轨的时间线与合成优先级。
type TrackSlots struct {
	Track    string
	Priority int // 数值小者优先
	FPS      float64
	Slots    []media.Slot
}

// PlanFrame 输出时间线上的一帧计划。
type PlanFrame struct {
	OutSeq int
	TimeMs float64
	Track  string // 空表示黑帧
	File   string
	Hash   string
	Black  bool
	GapSrc bool // 命中的最高优先级来源为 GAP
	BadSrc bool // 源帧哈希坏被隔离
}

// BadRange 坏哈希帧影响的输出帧范围。
type BadRange struct {
	Track   string
	Seq     int64
	FromOut int
	ToOut   int
}

// Plan 合成计划与统计。
type Plan struct {
	Frames     []PlanFrame
	FPS        float64
	AlignMaxMs float64
	GapFrames  int
	BadRanges  []BadRange
}

// Compose 按轨道优先级确定性合成：同一输出时刻取优先级最高的有效帧，
// GAP 与坏哈希帧不作为画面来源，无来源时输出黑帧。
func Compose(tracks []TrackSlots, fps float64) (*Plan, error) {
	if fps <= 0 {
		return nil, fmt.Errorf("render: invalid output fps %v", fps)
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("render: no tracks")
	}
	ts := make([]TrackSlots, len(tracks))
	copy(ts, tracks)
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Priority != ts[j].Priority {
			return ts[i].Priority < ts[j].Priority
		}
		return ts[i].Track < ts[j].Track
	})
	minStart, maxEnd, err := extent(ts)
	if err != nil {
		return nil, err
	}
	outDur := 1000 / fps
	n := int((maxEnd-minStart)/outDur + 0.5)
	if n < 1 {
		n = 1
	}
	// alignErr 返回源帧起点相对输出网格的偏差（对齐误差）。
	alignErr := func(startMs float64) float64 {
		grid := minStart + float64(int((startMs-minStart)/outDur+0.5))*outDur
		return abs(startMs - grid)
	}
	plan := &Plan{FPS: fps, Frames: make([]PlanFrame, n)}
	for k := 0; k < n; k++ {
		t := minStart + float64(k)*outDur
		pf := PlanFrame{OutSeq: k, TimeMs: t, Black: true}
		for _, tr := range ts {
			s := findSlot(tr.Slots, t)
			if s == nil {
				continue
			}
			switch {
			case s.Gap:
				pf.GapSrc = true // 记录但继续找低优先级轨
			case s.Bad:
				// 坏哈希帧隔离：不作为来源，合并记录影响范围
				if n := len(plan.BadRanges); n > 0 &&
					plan.BadRanges[n-1].Track == s.Track &&
					plan.BadRanges[n-1].Seq == s.Seq &&
					plan.BadRanges[n-1].ToOut == k-1 {
					plan.BadRanges[n-1].ToOut = k
				} else {
					plan.BadRanges = append(plan.BadRanges, BadRange{Track: s.Track, Seq: s.Seq, FromOut: k, ToOut: k})
				}
			default:
				pf.Track, pf.File, pf.Hash, pf.Black = s.Track, s.File, s.Hash, false
				if d := alignErr(s.StartMs); d > plan.AlignMaxMs {
					plan.AlignMaxMs = d
				}
			}
			if pf.Track != "" {
				break
			}
		}
		if pf.GapSrc && pf.Track == "" {
			plan.GapFrames++
		}
		plan.Frames[k] = pf
	}
	return plan, nil
}

func extent(ts []TrackSlots) (minStart, maxEnd float64, err error) {
	first := true
	for _, tr := range ts {
		for _, s := range tr.Slots {
			if first || s.StartMs < minStart {
				minStart = s.StartMs
			}
			if e := s.StartMs + s.DurMs; first || e > maxEnd {
				maxEnd = e
			}
			first = false
		}
	}
	if first {
		return 0, 0, fmt.Errorf("render: empty timeline")
	}
	return minStart, maxEnd, nil
}

// findSlot 返回覆盖时刻 t 的槽（slots 按 StartMs 升序）。
func findSlot(slots []media.Slot, t float64) *media.Slot {
	i := sort.Search(len(slots), func(i int) bool { return slots[i].StartMs > t }) - 1
	if i < 0 {
		return nil
	}
	s := &slots[i]
	if t < s.StartMs+s.DurMs {
		return s
	}
	return nil
}

// Loader 按文件相对路径加载源图像。
type Loader func(file string) (image.Image, error)

// Render 执行合成计划，返回输出帧序列；无来源帧以黑帧补齐。
// 源图像缓存有界（maxCacheEntries）。
func Render(plan *Plan, load Loader, width, height int) ([]image.Image, error) {
	if plan == nil || len(plan.Frames) == 0 {
		return nil, fmt.Errorf("render: empty plan")
	}
	cache := map[string]image.Image{}
	var order []string
	out := make([]image.Image, len(plan.Frames))
	for i, pf := range plan.Frames {
		if pf.Black {
			out[i] = BlackFrame(width, height)
			continue
		}
		img, ok := cache[pf.File]
		if !ok {
			loaded, err := load(pf.File)
			if err != nil {
				return nil, fmt.Errorf("render: out frame %d: %w", pf.OutSeq, err)
			}
			img = imaging.Resize(loaded, width, height, imaging.Lanczos)
			if len(cache) >= maxCacheEntries {
				delete(cache, order[0])
				order = order[1:]
			}
			cache[pf.File] = img
			order = append(order, pf.File)
		}
		out[i] = img
	}
	return out, nil
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
