package graph

import (
	"errors"
	"testing"

	"bcastqc/internal/model"
)

func TestTopoSortLinearChain(t *testing.T) {
	p := NewPlan(1)
	for i := 0; i < 4; i++ {
		if err := p.AddNode(TrackNode{ID: i, SampleRate: 48000, TotalSamples: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	// 3 依赖 2 依赖 1 依赖 0。
	if err := p.AddEdge(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.AddEdge(2, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.AddEdge(3, 2); err != nil {
		t.Fatal(err)
	}
	order, err := p.TopoSort()
	if err != nil {
		t.Fatalf("线性链不应报错: %v", err)
	}
	want := []int{0, 1, 2, 3}
	for i, v := range order {
		if v != want[i] {
			t.Fatalf("拓扑序错误，得到 %v 期望 %v", order, want)
		}
	}
}

func TestTopoSortCycle(t *testing.T) {
	p := NewPlan(0)
	for i := 0; i < 3; i++ {
		_ = p.AddNode(TrackNode{ID: i, SampleRate: 48000, TotalSamples: 100})
	}
	_ = p.AddEdge(0, 1)
	_ = p.AddEdge(1, 2)
	_ = p.AddEdge(2, 0)
	_, err := p.TopoSort()
	if !errors.Is(err, model.ErrGraphCycle) {
		t.Fatalf("环路应报 ErrGraphCycle，实际 %v", err)
	}
}

func TestSelfLoopRejected(t *testing.T) {
	p := NewPlan(0)
	_ = p.AddNode(TrackNode{ID: 1, SampleRate: 48000, TotalSamples: 100})
	if err := p.AddEdge(1, 1); !errors.Is(err, model.ErrGraphCycle) {
		t.Fatalf("自环应报 ErrGraphCycle，实际 %v", err)
	}
}

func TestDuplicateNodeAndUnknownEdge(t *testing.T) {
	p := NewPlan(0)
	node := TrackNode{ID: 1, SampleRate: 48000, TotalSamples: 100}
	if err := p.AddNode(node); err != nil {
		t.Fatal(err)
	}
	if err := p.AddNode(node); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("重复节点应报 ErrInvalidInput: %v", err)
	}
	if err := p.AddEdge(1, 99); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("未知依赖应报 ErrInvalidInput: %v", err)
	}
}

func TestClockDriftDetection(t *testing.T) {
	// 同采样率、同长度：无漂移。
	p := NewPlan(1)
	_ = p.AddNode(TrackNode{ID: 0, SampleRate: 48000, TotalSamples: 48000})
	_ = p.AddNode(TrackNode{ID: 1, SampleRate: 48000, TotalSamples: 48000})
	if err := p.CheckClockDrift(); err != nil {
		t.Fatalf("等长轨道不应报漂移: %v", err)
	}
	// 末端相差 10 个采样，容差 1：应报漂移。
	p2 := NewPlan(1)
	_ = p2.AddNode(TrackNode{ID: 0, SampleRate: 48000, TotalSamples: 48000})
	_ = p2.AddNode(TrackNode{ID: 1, SampleRate: 48000, TotalSamples: 48010})
	if err := p2.CheckClockDrift(); !errors.Is(err, model.ErrClockDrift) {
		t.Fatalf("超容差漂移应报 ErrClockDrift，实际 %v", err)
	}
}

func TestSampleRangeCheck(t *testing.T) {
	p := NewPlan(0)
	_ = p.AddNode(TrackNode{ID: 0, SampleRate: 48000, TotalSamples: 1000})
	if err := p.CheckSampleRange(0, 999); err != nil {
		t.Fatalf("合法采样点不应报错: %v", err)
	}
	if err := p.CheckSampleRange(0, 1001); !errors.Is(err, model.ErrOutOfRange) {
		t.Fatalf("越界采样应报 ErrOutOfRange: %v", err)
	}
	if err := p.CheckSampleRange(0, -1); !errors.Is(err, model.ErrOutOfRange) {
		t.Fatalf("负采样应报 ErrOutOfRange: %v", err)
	}
	if err := p.CheckSampleRange(99, 0); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("未知轨道应报 ErrInvalidInput: %v", err)
	}
}
