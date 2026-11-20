// Package pipeline 编排处理流水线：解析 → 统一时间基准 → 轨道依赖图 →
// 切分/静音/指纹 → 检查点与溯源。结果确定性由种子与输入摘要保证。
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediagraph/internal/checkpoint"
	"mediagraph/internal/graph"
	"mediagraph/internal/provenance"
	"mediagraph/internal/testkit"
	"mediagraph/internal/timeline"
)

// 安全约束：限制输入大小、轨道数与单轨时间跨度，防止恶意长度字段。
const (
	MaxInputBytes = 16 << 20
	MaxCues       = 100000
	MaxTracks     = 64
)

// MaxSpanTicks 为单轨最大时间跨度（24 小时）。
const MaxSpanTicks = timeline.TicksPerDay

// Config 为一次运行的参数。
type Config struct {
	Workdir        string
	Seed           int64
	InterruptAfter int // 完成 N 个步骤后中断；-1 不中断
	WindowMs       int64
	MinWindowMs    int64
}

// Params 返回参与检查点绑定的参数表。
func (c *Config) Params() map[string]string {
	return map[string]string{
		"seed":       fmt.Sprint(c.Seed),
		"window_ms":  fmt.Sprint(c.WindowMs),
		"min_win_ms": fmt.Sprint(c.MinWindowMs),
	}
}

// Step 为一个流水线节点。
type Step struct {
	ID    string
	Track string
	Deps  []string
	Run   func(ctx *Ctx) error
}

// Ctx 为步骤执行上下文。
type Ctx struct {
	Cfg   *Config
	State *checkpoint.State
	Log   *provenance.Log
	Fx    *testkit.Fixtures
}

// Graph 构造显式轨道依赖图。
func Graph(steps []Step) *graph.Graph {
	g := graph.New()
	for _, s := range steps {
		g.Add(s.ID, s.Deps...)
	}
	return g
}

// ReadInput 读取输入文件并强制执行大小约束。
func ReadInput(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxInputBytes {
		return nil, fmt.Errorf("input %s too large: %d > %d", name, info.Size(), MaxInputBytes)
	}
	return os.ReadFile(path)
}

// Run 按依赖序执行步骤：已完成的步骤跳过（断点续跑），
// 依赖失败的节点不执行，达到 interruptAfter 后中断并保存检查点。
// 返回本轮执行的步骤、是否中断、可恢复错误数。
func Run(ctx *Ctx, steps []Step, interruptAfter int) (ran []string, interrupted bool, err error) {
	g := Graph(steps)
	byID := map[string]Step{}
	for _, s := range steps {
		byID[s.ID] = s
	}
	completed := 0
	_, _, err = g.Plan(func(id string) error {
		s := byID[id]
		if ctx.State.IsDone(id) {
			return nil // 断点续跑：复用检查点结果
		}
		if interruptAfter >= 0 && completed >= interruptAfter {
			return errInterrupt
		}
		if err := s.Run(ctx); err != nil {
			return err
		}
		completed++
		ran = append(ran, id)
		return nil
	})
	if err != nil {
		return ran, false, err
	}
	// 检查是否因中断而留有未完成节点
	for _, s := range steps {
		if !ctx.State.IsDone(s.ID) {
			interrupted = true
		}
	}
	return ran, interrupted, nil
}

// errInterrupt 为内部中断信号：标记节点失败以阻断其依赖，本身视为可恢复。
var errInterrupt = fmt.Errorf("interrupted")

// IsInterrupt 报告错误是否为中断信号。
func IsInterrupt(err error) bool {
	return err == errInterrupt
}
