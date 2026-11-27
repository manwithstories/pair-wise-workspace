package pipeline

import (
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/fixture"
	"medpipe/internal/ingest"
	"medpipe/internal/manifest"
)

func buildOpts(t *testing.T, out string, fault ingest.Fault, stopAfter int) Options {
	t.Helper()
	input := filepath.Join(out, "input")
	spec := fixture.Spec{Seed: 41, Tracks: 3, Seconds: 2, SampleRate: 48000,
		FPS: 10, BlockFrames: 4800, Bits: 16}
	if _, err := fixture.Generate(input, spec); err != nil {
		t.Fatal(err)
	}
	return Options{
		Seed: 41, Tracks: 3, Seconds: 2, FPSNum: 10, FPSDen: 1,
		TargetBPS: 1_000_000, OutDir: filepath.Join(out, "run"), InputDir: input,
		Fault: fault, StopAfter: stopAfter, WindowSize: 16, PacketFrames: 480,
	}
}

func TestFullRunProducesManifest(t *testing.T) {
	opts := buildOpts(t, t.TempDir(), ingest.FaultNone, 0)
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run()
	if err != nil {
		t.Fatalf("完整运行失败: %v", err)
	}
	if res.Summary.Frames != 20 {
		t.Fatalf("应渲染 20 帧, 得 %d", res.Summary.Frames)
	}
	if res.Summary.MaxErrorSamples > 1 {
		t.Fatalf("时间对齐误差超 1 采样: %d", res.Summary.MaxErrorSamples)
	}
	mf, err := manifest.Load(filepath.Join(opts.OutDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mf.Verify(opts.OutDir); err != nil {
		t.Fatalf("清单帧哈希校验失败: %v", err)
	}
}

func TestResumeMatchesFullRun(t *testing.T) {
	dir := t.TempDir()
	// 1) 带突发丢失，处理 2 块后中断
	opts := buildOpts(t, dir, ingest.FaultBurstLoss, 2)
	r, _ := New(opts)
	res, err := r.Run()
	if clock.CodeOf(err) != clock.CodeInterrupted || !res.Interrupted {
		t.Fatalf("应返回 interrupted, 得 %v", err)
	}
	// 2) 续跑完成
	opts.Resume = true
	opts.StopAfter = 0
	r2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := r2.Run()
	if err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	// 3) 同参数完整运行（无中断）作为对照
	fullDir := t.TempDir()
	fullOpts := buildOpts(t, fullDir, ingest.FaultBurstLoss, 0)
	fullRunner, err := New(fullOpts)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullRunner.Run()
	if err != nil {
		t.Fatalf("对照运行失败: %v", err)
	}
	if resumed.Summary.FrameSHA256 != full.Summary.FrameSHA256 {
		t.Fatal("续跑与完整运行的链式帧哈希不一致")
	}
	if resumed.Summary.ClippedSamples != full.Summary.ClippedSamples ||
		resumed.Summary.MissingSamples != full.Summary.MissingSamples {
		t.Fatal("续跑音频统计与完整运行不一致")
	}
	mf, _ := manifest.Load(filepath.Join(opts.OutDir, "manifest.json"))
	if err := mf.Verify(opts.OutDir); err != nil {
		t.Fatalf("续跑清单校验失败: %v", err)
	}
}

func TestConflictRejectsManifest(t *testing.T) {
	opts := buildOpts(t, t.TempDir(), ingest.FaultConflict, 0)
	r, _ := New(opts)
	_, err := r.Run()
	if clock.CodeOf(err) != clock.CodeConflict {
		t.Fatalf("冲突应报 conflict, 得 %v", err)
	}
	if clock.BlockOf(err) < 0 {
		t.Fatalf("冲突错误必须报告块号, 得 %d", clock.BlockOf(err))
	}
}

func TestCheckpointParamChangeRejected(t *testing.T) {
	dir := t.TempDir()
	opts := buildOpts(t, dir, ingest.FaultBurstLoss, 2)
	r0, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r0.Run(); clock.CodeOf(err) != clock.CodeInterrupted {
		t.Fatal("预期中断")
	}
	opts.Resume = true
	opts.StopAfter = 0
	opts.TargetBPS = 2_000_000 // 参数变化
	r3, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r3.Run(); clock.CodeOf(err) != clock.CodeCheckpoint {
		t.Fatalf("参数变化应使检查点失效, 得 %v", err)
	}
}
