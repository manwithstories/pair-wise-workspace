package pipeline

import (
	"path/filepath"
	"testing"

	"archivecheck/internal/fixture"
	"archivecheck/internal/transport"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	p := fixture.DefaultParams()
	p.Frames = 1200
	p.CorruptFrame = -1
	return Config{
		Params:          p,
		SegmentTargetNS: 2_000_000_000,
		BoundaryTolNS:   100_000_000,
		WindowSize:      64,
		Fault: transport.FaultConfig{
			LossEvery: 97, ReorderEvery: 131, DupeEvery: 53,
		},
	}
}

func TestRunSuccessMetrics(t *testing.T) {
	rep, err := Run(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Published || !rep.OK {
		t.Fatalf("成功路径应发布清单: %+v", rep)
	}
	if rep.SyncErrorNS > 20_000_000 {
		t.Fatalf("同步误差 %dns 超过 20ms", rep.SyncErrorNS)
	}
	if rep.RecoveryRate < 0.95 {
		t.Fatalf("恢复率 %.3f 低于 0.95", rep.RecoveryRate)
	}
	if rep.P95LatencyNS >= 100_000_000 {
		t.Fatalf("P95 延迟 %dns 不低于 100ms", rep.P95LatencyNS)
	}
	if rep.BoundaryMiss != 0 {
		t.Fatalf("边界不匹配应为 0, got %d", rep.BoundaryMiss)
	}
	if rep.Segments == 0 || len(rep.ManifestHash) != 64 {
		t.Fatalf("分片或清单哈希异常: %d %q", rep.Segments, rep.ManifestHash)
	}
}

func TestRunDeterministicManifestHash(t *testing.T) {
	c := testConfig(t)
	r1, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ManifestHash != r2.ManifestHash {
		t.Fatalf("重复运行清单哈希不一致: %s vs %s", r1.ManifestHash, r2.ManifestHash)
	}
}

func TestRunCorruptFrameRejects(t *testing.T) {
	c := testConfig(t)
	c.Params.CorruptFrame = 811
	r, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	if r.Published {
		t.Fatal("损坏帧路径不得发布清单")
	}
	if r.FirstError == nil || r.FirstError.Seq != 811 {
		t.Fatalf("应定位损坏帧 811: %+v", r.FirstError)
	}
}

func TestCrossModuleFaultRejectedAndDeterministic(t *testing.T) {
	c := testConfig(t)
	c.CrossModuleFault = true
	r1, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Published || r1.BoundaryMiss == 0 {
		t.Fatalf("跨模块故障必须拒绝发布且记录边界冲突: %+v", r1)
	}
	r2, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Published {
		t.Fatal("重跑仍必须拒绝发布")
	}
}

func TestTailTruncationRejected(t *testing.T) {
	c := testConfig(t)
	c.TailTruncate = true
	r, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	if r.Published || r.RejectedReason != "tail_truncated" {
		t.Fatalf("尾截断必须拒绝并标记 tail_truncated: %+v", r)
	}
	// 清单中确实存在不完整尾分片。
	for name, m := range r.ManifestByRep {
		last := m.Segments[len(m.Segments)-1]
		if !last.TailIncomplete {
			t.Fatalf("%s 尾分片应标记不完整", name)
		}
	}
}

func TestCheckpointResumeSameHash(t *testing.T) {
	c := testConfig(t)
	c.CheckpointPath = filepath.Join(t.TempDir(), "cp.json")
	full, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	// 写入一个与当前输入匹配的检查点，验证续跑被接受且结果哈希一致。
	if err := seedCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	resumed, err := Run(c)
	if err != nil {
		t.Fatalf("匹配检查点应允许续跑: %v", err)
	}
	if !resumed.CheckpointUsed {
		t.Fatal("应识别并使用检查点")
	}
	if resumed.ManifestHash != full.ManifestHash {
		t.Fatalf("续跑清单哈希必须与无中断运行一致: %s vs %s",
			resumed.ManifestHash, full.ManifestHash)
	}
}

func TestCheckpointMismatchRejected(t *testing.T) {
	c := testConfig(t)
	c.CheckpointPath = filepath.Join(t.TempDir(), "cp.json")
	if err := seedCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	c.Params.DriftPPM++ // 改变输入，使检查点摘要失效
	if _, err := Run(c); err != ErrCheckpointMismatch {
		t.Fatalf("摘要不匹配必须拒绝续跑, got %v", err)
	}
}
