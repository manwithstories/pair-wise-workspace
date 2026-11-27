package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bcastqc/internal/model"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if s.Exists() {
		t.Fatal("新存储不应已存在检查点")
	}
	st := &State{
		JobID: "job-1", Stage: StageAnalyze,
		Steps:            map[string]int64{"analyze": 4},
		TrackSamples:     map[int]int64{0: 1000, 1: 2000},
		RecoveredSamples: 3000,
		OutputSHA:        "abc",
		RunCount:         1,
	}
	if err := s.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !s.Exists() {
		t.Fatal("保存后检查点应存在")
	}
	got, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("Load: %v ok=%v", err, ok)
	}
	if got.JobID != "job-1" || got.RecoveredSamples != 3000 || got.RunCount != 1 {
		t.Fatalf("往返数据不一致: %+v", got)
	}
	if got.TrackSamples[1] != 2000 {
		t.Fatalf("轨道采样数丢失: %+v", got.TrackSamples)
	}
}

func TestLoadMissingReturnsNotOK(t *testing.T) {
	s := NewStore(t.TempDir())
	st, ok, err := s.Load()
	if err != nil || ok {
		t.Fatalf("不存在的检查点应返回 ok=false，得到 ok=%v err=%v", ok, err)
	}
	if st.Steps == nil || st.TrackSamples == nil {
		t.Fatal("返回状态应初始化好 map")
	}
}

func TestAtomicReplaceLeavesNoTmp(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for i := 0; i < 5; i++ {
		if err := s.Save(&State{JobID: "j", Stage: StageIngest, RunCount: i}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("原子替换后不应残留临时文件: %s", e.Name())
		}
	}
}

func TestCorruptCheckpointDetected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(s.Path(), []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Load()
	if !errors.Is(err, model.ErrCheckpoint) {
		t.Fatalf("损坏检查点应报 ErrCheckpoint，实际 %v", err)
	}
}

func TestVerifySampleCounts(t *testing.T) {
	st := &State{TrackSamples: map[int]int64{0: 100, 1: 200}}
	if err := VerifySampleCounts(st, map[int]int64{0: 100, 1: 200}); err != nil {
		t.Fatalf("一致采样数不应报错: %v", err)
	}
	if err := VerifySampleCounts(st, map[int]int64{0: 100, 1: 999}); !errors.Is(err, model.ErrUnrecoverable) {
		t.Fatalf("采样数变化应报 ErrUnrecoverable: %v", err)
	}
	if err := VerifySampleCounts(st, map[int]int64{0: 100}); !errors.Is(err, model.ErrUnrecoverable) {
		t.Fatalf("轨道缺失应报 ErrUnrecoverable: %v", err)
	}
	// 空检查点不约束。
	if err := VerifySampleCounts(&State{}, map[int]int64{0: 1}); err != nil {
		t.Fatalf("空检查点应放行: %v", err)
	}
}

func TestReset(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Save(&State{JobID: "j", Stage: StageComplete})
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if s.Exists() {
		t.Fatal("Reset 后检查点应消失")
	}
	if err := s.Reset(); err != nil {
		t.Fatalf("对不存在文件 Reset 应幂等，实际 %v", err)
	}
}
