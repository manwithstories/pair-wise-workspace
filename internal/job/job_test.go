package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateMachineLegalAndIllegal(t *testing.T) {
	log := filepath.Join(t.TempDir(), "events.log")
	m := NewMachine(log, StatePending)
	for _, st := range []State{StatePlanning, StateEncoding, StateVerifying, StateDone} {
		if err := m.Transition(st); err != nil {
			t.Fatalf("合法迁移 %s 失败: %v", st, err)
		}
	}
	m2 := NewMachine(log, StatePending)
	if err := m2.Transition(StateDone); err == nil {
		t.Fatal("非法迁移 pending->done 未报错")
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "illegal_transition") {
		t.Fatal("非法迁移未写入事件日志")
	}
}

func TestJournalSaveLoadAndFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.state")
	j1 := &Journal{Version: 1, State: StateEncoding, Segments: []SegCheckpoint{{Index: 0, Status: "done", QP: 4.25, Bits: 123}}}
	if err := Save(path, j1); err != nil {
		t.Fatal(err)
	}
	j2 := &Journal{Version: 1, State: StateDone, Segments: []SegCheckpoint{{Index: 0, Status: "done"}, {Index: 1, Status: "done"}}}
	if err := Save(path, j2); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(path)
	if err != nil || !ok || got.State != StateDone {
		t.Fatalf("加载失败 ok=%v err=%v", ok, err)
	}
	// 模拟半写：主文件损坏，应回退到 .bak（j1 的内容）
	if err := os.WriteFile(path, []byte(`{"version": broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err = Load(path)
	if err != nil || !ok {
		t.Fatalf("回退失败 ok=%v err=%v", ok, err)
	}
	if got.State != StateEncoding || len(got.Segments) != 1 || got.Segments[0].Bits != 123 {
		t.Fatalf("回退内容不符: %+v", got)
	}
	// 无检查点时返回零值
	if _, ok, _ := Load(filepath.Join(dir, "none.state")); ok {
		t.Fatal("不存在的检查点不应返回 ok")
	}
}
