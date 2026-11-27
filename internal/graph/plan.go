// Package graph 维护显式轨道依赖图，提供拓扑排序、环路检测、
// 跨轨时钟漂移核验与采样点越界检查。
package graph

import (
	"fmt"

	"bcastqc/internal/model"
)

// TrackNode 描述图中的一条轨道节点。
type TrackNode struct {
	ID         int
	SampleRate uint32
	// TotalSamples 为该轨道声明的总采样点数。
	TotalSamples int64
}

// Plan 是轨道依赖图：Edges[a] 包含 b 表示 a 依赖 b（b 须先于 a 处理）。
type Plan struct {
	Nodes map[int]*TrackNode
	// Edges 以依赖方向存储：from 依赖 to。
	Edges map[int]map[int]bool
	// DriftToleranceSamples 为允许的跨轨末端采样点偏差上界。
	DriftToleranceSamples int64
}

// NewPlan 创建空依赖图并设定漂移容差（采样点）。
func NewPlan(driftTolerance int64) *Plan {
	if driftTolerance < 0 {
		driftTolerance = 0
	}
	return &Plan{
		Nodes:                 make(map[int]*TrackNode),
		Edges:                 make(map[int]map[int]bool),
		DriftToleranceSamples: driftTolerance,
	}
}

// AddNode 注册轨道节点，重复 ID 报无效输入。
func (p *Plan) AddNode(n TrackNode) error {
	if _, ok := p.Nodes[n.ID]; ok {
		return fmt.Errorf("%w: 轨道 %d 重复注册", model.ErrInvalidInput, n.ID)
	}
	if n.SampleRate == 0 {
		return fmt.Errorf("%w: 轨道 %d 采样率为 0", model.ErrInvalidInput, n.ID)
	}
	if err := model.CheckSampleCount(n.TotalSamples); err != nil {
		return fmt.Errorf("轨道 %d: %w", n.ID, err)
	}
	p.Nodes[n.ID] = &n
	p.Edges[n.ID] = make(map[int]bool)
	return nil
}

// AddEdge 登记依赖：from 依赖 to。引用未知节点或自环均报错。
func (p *Plan) AddEdge(from, to int) error {
	if from == to {
		return fmt.Errorf("%w: 轨道 %d 存在自环", model.ErrGraphCycle, from)
	}
	if _, ok := p.Nodes[from]; !ok {
		return fmt.Errorf("%w: 依赖起点轨道 %d 不存在", model.ErrInvalidInput, from)
	}
	if _, ok := p.Nodes[to]; !ok {
		return fmt.Errorf("%w: 依赖终点轨道 %d 不存在", model.ErrInvalidInput, to)
	}
	p.Edges[from][to] = true
	return nil
}

// TopoSort 基于 Kahn 算法输出依赖拓扑序；存在环路时返回 ErrGraphCycle
// 并附带环上的节点 ID。
func (p *Plan) TopoSort() ([]int, error) {
	indeg := make(map[int]int, len(p.Nodes))
	for id := range p.Nodes {
		indeg[id] = 0
	}
	for from, deps := range p.Edges {
		for to := range deps {
			if from == to {
				continue
			}
			indeg[from]++ // from 的入度 = 它依赖的节点数。
			_ = to
		}
	}
	// 稳定的零入度队列：按 ID 排序保证输出确定性。
	queue := make([]int, 0)
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	sortInts(queue)

	order := make([]int, 0, len(p.Nodes))
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		order = append(order, cur)
		// 释放所有“依赖 cur”的节点。
		for from, deps := range p.Edges {
			if deps[cur] {
				indeg[from]--
				if indeg[from] == 0 {
					queue = append(queue, from)
					sortInts(queue)
				}
			}
		}
	}
	if len(order) != len(p.Nodes) {
		cyclic := p.cycleNodes(indeg)
		return nil, fmt.Errorf("%w: 依赖成环，环上节点 %v", model.ErrGraphCycle, cyclic)
	}
	return order, nil
}

// cycleNodes 返回仍有正入度（即陷入环中）的节点 ID。
func (p *Plan) cycleNodes(indeg map[int]int) []int {
	out := make([]int, 0)
	for id, d := range indeg {
		if d > 0 {
			out = append(out, id)
		}
	}
	sortInts(out)
	return out
}

// CheckClockDrift 核验所有轨道在统一时间基上的末端偏差。
// 各轨道采样率可能不同，先换算为标称时长（纳秒）再比对；
// 偏差超过容差对应的采样点数即报 ErrClockDrift。
func (p *Plan) CheckClockDrift() error {
	var refID int
	var refNs int64
	first := true
	for id, n := range p.Nodes {
		ns, err := model.Mul64Checked(n.TotalSamples, 1_000_000_000)
		if err != nil {
			return fmt.Errorf("轨道 %d 时长换算溢出: %w", id, err)
		}
		ns /= int64(n.SampleRate)
		if first {
			refID, refNs, first = id, ns, false
			continue
		}
		diff := ns - refNs
		if diff < 0 {
			diff = -diff
		}
		// 用参考轨道采样率把纳秒差折回采样点数。
		ref := p.Nodes[refID]
		scaled, err := model.Mul64Checked(diff, int64(ref.SampleRate))
		if err != nil {
			return fmt.Errorf("轨道 %d 漂移换算溢出: %w", id, err)
		}
		sampleDiff := scaled / 1_000_000_000
		if sampleDiff > p.DriftToleranceSamples {
			return fmt.Errorf("%w: 轨道 %d 与参考轨道 %d 漂移 %d 采样（容差 %d）",
				model.ErrClockDrift, id, refID, sampleDiff, p.DriftToleranceSamples)
		}
	}
	return nil
}

// CheckSampleRange 核验给定采样点是否落在指定轨道的合法范围内。
func (p *Plan) CheckSampleRange(trackID, sample int64) error {
	n, ok := p.Nodes[int(trackID)]
	if !ok {
		return fmt.Errorf("%w: 轨道 %d 不在依赖图中", model.ErrInvalidInput, trackID)
	}
	if sample < 0 || sample > n.TotalSamples {
		return fmt.Errorf("%w: 轨道 %d 采样点 %d 越界 [0,%d]",
			model.ErrOutOfRange, trackID, sample, n.TotalSamples)
	}
	return nil
}

// sortInts 为小切片提供无依赖的插入排序，保持输出确定性。
func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
