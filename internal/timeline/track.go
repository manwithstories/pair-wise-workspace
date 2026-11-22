package timeline

import (
	"fmt"
	"sort"
)

// 轨道类别。
const (
	KindVideo    = "video"
	KindAudio    = "audio"
	KindSubtitle = "subtitle"
	KindChapter  = "chapter"
	KindEvent    = "event" // ID3 / emsg / SEI / RTP 事件轨
)

// Track 描述一条媒体、字幕或事件轨道及其在共同时间线上的依赖关系。
// Deps 为本轨道关联所依赖的轨道 ID（例如事件轨依赖其挂载的视频轨）。
type Track struct {
	ID       uint32
	Kind     string
	Label    string // 机位/来源标签，如 camA
	Codec    string
	Clock    *Clock
	Deps     []uint32
	Duration int64 // 共同时间线纳秒
}

// Graph 是轨道依赖图。解析顺序由拓扑排序决定：媒体时基与样本先于跨轨事件。
type Graph struct {
	tracks map[uint32]*Track
	order  []uint32
}

func NewGraph() *Graph {
	return &Graph{tracks: make(map[uint32]*Track)}
}

func (g *Graph) Add(t *Track) error {
	if t == nil {
		return fmt.Errorf("timeline: cannot add nil track")
	}
	if _, ok := g.tracks[t.ID]; ok {
		return fmt.Errorf("timeline: duplicate track id %d", t.ID)
	}
	g.tracks[t.ID] = t
	g.order = append(g.order, t.ID)
	return nil
}

func (g *Graph) Get(id uint32) (*Track, bool) {
	t, ok := g.tracks[id]
	return t, ok
}

// IDs 按加入顺序返回全部轨道 ID。
func (g *Graph) IDs() []uint32 {
	out := make([]uint32, len(g.order))
	copy(out, g.order)
	return out
}

func (g *Graph) Len() int { return len(g.tracks) }

// Sorted 稳定排序：先按 kind 固定优先级，再按 ID，保证重跑顺序确定。
func (g *Graph) Sorted() []*Track {
	rank := map[string]int{
		KindVideo: 0, KindAudio: 1, KindSubtitle: 2,
		KindChapter: 3, KindEvent: 4,
	}
	ids := g.IDs()
	sort.Slice(ids, func(i, j int) bool {
		a, b := g.tracks[ids[i]], g.tracks[ids[j]]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		return a.ID < b.ID
	})
	out := make([]*Track, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.tracks[id])
	}
	return out
}

// CycleError 携带检测到的循环依赖路径。
type CycleError struct {
	Path []uint32
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("timeline: cyclic track dependency at %v", e.Path)
}

// TopoSort 返回满足依赖在前的轨道顺序；存在循环时返回 CycleError 及路径。
func (g *Graph) TopoSort() ([]*Track, error) {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[uint32]int, len(g.order))
	var stack []uint32
	var result []*Track
	var visit func(id uint32) error
	visit = func(id uint32) error {
		if color[id] == gray {
			// 从栈中抽取环路径。
			path := []uint32{id}
			for i := len(stack) - 1; i >= 0; i-- {
				path = append(path, stack[i])
				if stack[i] == id {
					break
				}
			}
			// 反转并闭合环。
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			path = append(path, id)
			return &CycleError{Path: path}
		}
		if color[id] == black {
			return nil
		}
		color[id] = gray
		stack = append(stack, id)
		deps := append([]uint32(nil), g.tracks[id].Deps...)
		sort.Slice(deps, func(i, j int) bool { return deps[i] < deps[j] })
		for _, d := range deps {
			if _, ok := g.tracks[d]; !ok {
				return fmt.Errorf("timeline: track %d depends on missing track %d", id, d)
			}
			if err := visit(d); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		result = append(result, g.tracks[id])
		return nil
	}
	for _, id := range g.IDs() { // 确定的起点顺序
		if color[id] == white {
			if err := visit(id); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
