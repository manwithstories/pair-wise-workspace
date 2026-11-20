package graph

import (
	"errors"
	"reflect"
	"testing"
)

func TestTopoSortDeterministic(t *testing.T) {
	g := New()
	g.Add("split", "subtitles", "timecode")
	g.Add("subtitles")
	g.Add("timecode", "media")
	g.Add("media")
	g.Add("silence", "transport")
	g.Add("transport")
	order, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"media", "subtitles", "timecode", "split", "transport", "silence"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order=%v want %v", order, want)
	}
	// 再次执行结果一致
	order2, _ := g.TopoSort()
	if !reflect.DeepEqual(order, order2) {
		t.Fatal("not deterministic")
	}
}

func TestCyclePath(t *testing.T) {
	g := New()
	g.Add("a", "c")
	g.Add("b", "a")
	g.Add("c", "b")
	_, err := g.TopoSort()
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("err=%v", err)
	}
	// 闭环路径首尾相同
	if len(ce.Path) < 2 || ce.Path[0] != ce.Path[len(ce.Path)-1] {
		t.Fatalf("path=%v", ce.Path)
	}
}

func TestUnknownDependency(t *testing.T) {
	g := New()
	g.Add("a", "ghost")
	if _, err := g.TopoSort(); err == nil {
		t.Fatal("expected error")
	}
}

func TestPlanSkipsFailedDependencies(t *testing.T) {
	g := New()
	g.Add("bad")
	g.Add("child", "bad")
	g.Add("grandchild", "child")
	g.Add("independent")
	var ran []string
	_, status, err := g.Plan(func(id string) error {
		ran = append(ran, id)
		if id == "bad" {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if status["bad"] != Failed || status["child"] != Skipped || status["grandchild"] != Skipped {
		t.Fatalf("status=%v", status)
	}
	if status["independent"] != OK {
		t.Fatalf("independent=%v", status["independent"])
	}
	// 被跳过的节点不得执行
	for _, id := range ran {
		if id == "child" || id == "grandchild" {
			t.Fatalf("skipped node %q executed", id)
		}
	}
}
