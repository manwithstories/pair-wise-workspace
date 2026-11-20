// Package graph 构造显式轨道依赖图并按依赖排序；
// 循环依赖给出闭环路径，依赖失败的节点不得执行，独立轨道仍可完成。
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Graph 为有向依赖图：边 dep -> node 表示 node 依赖 dep。
type Graph struct {
	deps map[string][]string
}

// New 创建空图。
func New() *Graph {
	return &Graph{deps: map[string][]string{}}
}

// Add 注册节点及其依赖；重复注册覆盖依赖。
func (g *Graph) Add(id string, deps ...string) {
	g.deps[id] = append([]string(nil), deps...)
}

// CycleError 携带闭环路径。
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	return "dependency cycle: " + strings.Join(e.Path, " -> ")
}

// Nodes 返回全部节点 ID（排序后，保证确定性）。
func (g *Graph) Nodes() []string {
	out := make([]string, 0, len(g.deps))
	for id := range g.deps {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Deps 返回节点依赖（未知节点视为无依赖）。
func (g *Graph) Deps(id string) []string {
	return g.deps[id]
}

// findCycle 用三色标记 DFS 查找一条闭环路径；无环返回 nil。
func (g *Graph) findCycle() []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = gray
		stack = append(stack, id)
		for _, d := range g.deps[id] {
			if _, known := g.deps[d]; !known {
				continue // 未注册依赖不参与成环
			}
			switch color[d] {
			case gray:
				// 找到环：从栈中 d 的位置到栈顶
				idx := 0
				for i, s := range stack {
					if s == d {
						idx = i
						break
					}
				}
				cycle = append(append([]string(nil), stack[idx:]...), d)
				return true
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	for _, id := range g.Nodes() {
		if color[id] == white && visit(id) {
			return cycle
		}
	}
	return nil
}

// TopoSort 返回确定性的依赖执行顺序（就绪集按字典序弹出）。
// 存在循环依赖时返回 *CycleError，Path 为闭环路径。
func (g *Graph) TopoSort() ([]string, error) {
	if c := g.findCycle(); c != nil {
		return nil, &CycleError{Path: c}
	}
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for id, ds := range g.deps {
		if _, ok := indeg[id]; !ok {
			indeg[id] = 0
		}
		for _, d := range ds {
			if _, known := g.deps[d]; !known {
				return nil, fmt.Errorf("node %q depends on unknown node %q", id, d)
			}
			indeg[id]++
			dependents[d] = append(dependents[d], id)
		}
	}
	var ready []string
	for id, n := range indeg {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dep := range dependents[id] {
			indeg[dep]--
			if indeg[dep] == 0 {
				// 插入并保持有序
				i := sort.SearchStrings(ready, dep)
				ready = append(ready, "")
				copy(ready[i+1:], ready[i:])
				ready[i] = dep
			}
		}
	}
	return order, nil
}

// Status 为节点执行状态。
type Status int

const (
	OK Status = iota
	Failed
	Skipped // 依赖失败/跳过，未执行
)

// Plan 按拓扑序执行：fn 返回 error 则节点失败；
// 任一依赖失败或被跳过的节点标记为 Skipped 且不执行 fn。独立分支继续执行。
func (g *Graph) Plan(fn func(id string) error) (order []string, status map[string]Status, err error) {
	order, err = g.TopoSort()
	if err != nil {
		return nil, nil, err
	}
	status = map[string]Status{}
	for _, id := range order {
		blocked := false
		for _, d := range g.deps[id] {
			if status[d] == Failed || status[d] == Skipped {
				blocked = true
				break
			}
		}
		if blocked {
			status[id] = Skipped
			continue
		}
		if fnErr := fn(id); fnErr != nil {
			status[id] = Failed
		} else {
			status[id] = OK
		}
	}
	return order, status, nil
}
