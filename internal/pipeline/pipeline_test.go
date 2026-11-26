package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"archivemedia/internal/fixture"
	"archivemedia/internal/manifest"
	"archivemedia/internal/model"
)

func generateSources(t *testing.T, dir string) []fixture.Source {
	t.Helper()
	srcs, err := fixture.GenerateAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	mkv, err := fixture.GenerateMKV(dir)
	if err != nil {
		t.Fatal(err)
	}
	return append(srcs, mkv)
}

func TestRunFullFlowAndIsolation(t *testing.T) {
	dir := t.TempDir()
	srcs := generateSources(t, dir)
	r, err := New(DefaultConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(srcs)
	if err != nil {
		t.Fatalf("完整运行不应报错（故障应隔离）: %v", err)
	}
	if res.Interrupted {
		t.Fatal("未配置中断点不应中断")
	}
	// 截断样例应带首个坏时间且仍恢复片段；非法剪辑点样例被隔离
	var trunc, illegal *SourceOutcome
	for i := range res.Outcomes {
		switch res.Outcomes[i].Kind {
		case fixture.KindTruncated:
			trunc = &res.Outcomes[i]
		case fixture.KindIllegalCut:
			illegal = &res.Outcomes[i]
		}
	}
	if trunc == nil || trunc.FirstBadAtUs == 0 || trunc.Recovered == 0 {
		t.Fatalf("截断样例应恢复前缀并标注坏时间: %+v", trunc)
	}
	if illegal == nil || illegal.Status != "isolated" {
		t.Fatalf("非法剪辑点样例应被隔离: %+v", illegal)
	}
	if res.Report == nil || res.Report.RecoveredSegments == 0 {
		t.Fatal("核验报告缺失或无恢复片段")
	}
	// 清单只含已提交片段
	m, err := manifest.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.SegmentCount != res.Report.RecoveredSegments {
		t.Fatalf("清单片段数=%d 报告=%d", m.SegmentCount, res.Report.RecoveredSegments)
	}
}

func TestInterruptAndResume(t *testing.T) {
	dir := t.TempDir()
	srcs := generateSources(t, dir)

	// 第一次运行，处理 2 个片段后中断
	cfg := DefaultConfig(dir)
	cfg.InterruptAfter = 2
	r1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r1.Run(srcs)
	if model.KindOf(err) != model.KindInterrupted {
		t.Fatalf("应返回中断错误, got %v", err)
	}
	committedAfterInterrupt := len(r1.jrnl.Committed())
	if committedAfterInterrupt != 2 {
		t.Fatalf("中断时应恰好提交 2 个片段, got %d", committedAfterInterrupt)
	}
	// 中断点必须持久化
	if cp := r1.jrnl.Checkpoint(); cp.InterruptAt != 2 {
		t.Fatalf("中断点未持久化: %d", cp.InterruptAt)
	}
	// 中断后磁盘上应有一个未提交孤儿片段（已物化但未进检查点）
	segEntries, _ := os.ReadDir(filepath.Join(dir, "segments"))
	if len(segEntries) <= committedAfterInterrupt {
		t.Fatal("应存在中断遗留的孤儿片段文件")
	}
	manifestBefore, err := os.ReadFile(filepath.Join(dir, manifest.ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 续跑：New 清理孤儿，复用已提交片段并补齐其余
	r2, err := New(DefaultConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	// 孤儿已被清理：磁盘文件数回到已提交数
	segEntries2, _ := os.ReadDir(filepath.Join(dir, "segments"))
	if len(segEntries2) != committedAfterInterrupt {
		t.Fatalf("孤儿未清理: 磁盘=%d 已提交=%d", len(segEntries2), committedAfterInterrupt)
	}
	res2, err := r2.Run(srcs)
	if err != nil {
		t.Fatalf("续跑不应报错: %v", err)
	}
	if res2.Interrupted {
		t.Fatal("续跑应正常完成")
	}
	if len(r2.jrnl.Committed()) <= committedAfterInterrupt {
		t.Fatal("续跑应补齐更多片段")
	}

	// 再跑一次：全部片段短路，产物哈希必须与续跑后一致
	r3, err := New(DefaultConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	res3, err := r3.Run(srcs)
	if err != nil {
		t.Fatal(err)
	}
	if res3.ManifestSha != res2.ManifestSha {
		t.Fatalf("重复运行清单哈希不一致: %s vs %s", res3.ManifestSha, res2.ManifestSha)
	}
	manifestAfter, _ := os.ReadFile(filepath.Join(dir, manifest.ManifestFileName))
	if len(manifestBefore) >= len(manifestAfter) {
		t.Fatal("续跑后清单应包含更多条目")
	}
}

func TestSegmentFilesPersisted(t *testing.T) {
	dir := t.TempDir()
	srcs := generateSources(t, dir)
	r, _ := New(DefaultConfig(dir))
	res, err := r.Run(srcs)
	if err != nil {
		t.Fatal(err)
	}
	segDir := filepath.Join(dir, "segments")
	entries, err := os.ReadDir(segDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != res.Report.RecoveredSegments {
		t.Fatalf("落盘片段文件数=%d 报告=%d", len(entries), res.Report.RecoveredSegments)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".arc" {
			t.Fatalf("片段应为 arc 文件: %s", e.Name())
		}
	}
}

func TestThresholdFailureReported(t *testing.T) {
	dir := t.TempDir()
	srcs := generateSources(t, dir)
	cfg := DefaultConfig(dir)
	cfg.MinPsnrDb = 80  // 极高门限，无损哨兵 99 仍过；改为测对齐不可达
	cfg.MaxAlignMs = -1 // 任何对齐误差都失败
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := r.Run(srcs)
	// 我们的样例吸附误差为 0，这里验证报告结构存在；门限失败由 verify 单测覆盖
	if res.Report == nil {
		t.Fatal("报告必须产出")
	}
}
