package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bcastqc/internal/analyze"
	"bcastqc/internal/checkpoint"
	"bcastqc/internal/graph"
	"bcastqc/internal/ingest"
	"bcastqc/internal/model"
)

func testConfig(t *testing.T, dir string, interrupt int) Config {
	t.Helper()
	return Config{
		JobID:          "test-job",
		OutDir:         dir,
		Seed:           41,
		Tracks:         4,
		Samples:        48000,
		Bandwidth:      []int64{1200000, 240000, 900000},
		Faults:         AllFaults(),
		InterruptAfter: interrupt,
		ReorderWindow:  4,
		DriftTolerance: 1,
	}
}

func TestBuildFixtureDeterministic(t *testing.T) {
	cfg := testConfig(t, t.TempDir(), 0)
	f1, err := BuildFixture(cfg)
	if err != nil {
		t.Fatalf("BuildFixture: %v", err)
	}
	f2, err := BuildFixture(cfg)
	if err != nil {
		t.Fatalf("BuildFixture 第二次: %v", err)
	}
	if len(f1.Tracks) != 4 {
		t.Fatalf("应生成 4 轨，实际 %d", len(f1.Tracks))
	}
	for i, tf := range f1.Tracks {
		if tf.ExpectFrames != 48000 {
			t.Fatalf("轨道 %d 期望帧数错误: %d", i, tf.ExpectFrames)
		}
		if string(tf.WAV) != string(f2.Tracks[i].WAV) {
			t.Fatalf("轨道 %d 的 WAV 夹具不确定", i)
		}
		// 每轨都应能被 ingest 重新核验通过。
		if _, err := ingest.ParseWAV(bytesReader(tf.WAV), tf.WAVSize); err != nil {
			t.Fatalf("轨道 %d 生成的 WAV 无法通过核验: %v", i, err)
		}
	}
	if len(f1.CycleEdges) != 2 {
		t.Fatalf("应生成成环样例边，实际 %v", f1.CycleEdges)
	}
}

func TestRiffOverflowSampleRejected(t *testing.T) {
	cfg := testConfig(t, t.TempDir(), 0)
	fix, err := BuildFixture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tf := range fix.Tracks {
		if len(tf.RiffOverflow) == 0 {
			t.Fatal("应生成 RIFF 长度溢出样例")
		}
		_, err := ingest.ParseWAV(bytesReader(tf.RiffOverflow), int64(len(tf.RiffOverflow)))
		if !errors.Is(err, model.ErrRiffStructure) {
			t.Fatalf("RIFF 溢出样例应被拒绝，实际 %v", err)
		}
	}
}

func TestCycleSampleDetected(t *testing.T) {
	cfg := testConfig(t, t.TempDir(), 0)
	fix, err := BuildFixture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan := graph.NewPlan(1)
	for _, tf := range fix.Tracks {
		if err := plan.AddNode(graph.TrackNode{
			ID: tf.ID, SampleRate: sampleRate, TotalSamples: tf.ExpectFrames,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range fix.CycleEdges {
		if err := plan.AddEdge(e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := plan.TopoSort(); !errors.Is(err, model.ErrGraphCycle) {
		t.Fatalf("成环应报 ErrGraphCycle，实际 %v", err)
	}
}

func TestRunFullPipelineAndReRunSHA(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir, 0)
	rep1, err := NewRunner(cfg).Run()
	if err != nil {
		t.Fatalf("首次运行失败: %v", err)
	}
	if rep1.Tracks != 4 || rep1.RecoveredSamples != 48000 {
		t.Fatalf("汇报数据错误: %+v", rep1)
	}
	if rep1.DamageSegments == 0 {
		t.Fatal("全部故障注入下应定位到损坏区段")
	}
	if rep1.LocErrorSamples > 1 {
		t.Fatalf("定位误差必须 ≤1 采样点，实际 %d", rep1.LocErrorSamples)
	}
	if len(rep1.SHA256) != 64 {
		t.Fatalf("SHA-256 应为 64 位十六进制，实际长度 %d", len(rep1.SHA256))
	}
	if rep1.ResumeMs >= 1000 {
		t.Fatalf("恢复耗时应低于 1000ms，实际 %dms", rep1.ResumeMs)
	}
	// 默认夹具每 4 包一个锚点，应有可计算的间隔且毫秒数为正。
	if rep1.AnchorNA || rep1.AnchorIntervals == 0 || rep1.AnchorMaxMs <= 0 {
		t.Fatalf("锚点间隔应可计算，实际 NA=%v count=%d maxMs=%f",
			rep1.AnchorNA, rep1.AnchorIntervals, rep1.AnchorMaxMs)
	}

	// 重复运行：样本数不变、SHA 完全一致。
	rep2, err := NewRunner(cfg).Run()
	if err != nil {
		t.Fatalf("重复运行失败: %v", err)
	}
	if !rep2.RerunSHAMatch || rep2.SHA256 != rep1.SHA256 {
		t.Fatalf("重复运行 SHA 不一致: %s vs %s", rep1.SHA256, rep2.SHA256)
	}
	if rep2.RecoveredSamples != rep1.RecoveredSamples {
		t.Fatalf("重复运行样本数变化 %d vs %d", rep1.RecoveredSamples, rep2.RecoveredSamples)
	}
}

func TestInterruptAndResume(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir, 2)
	_, err := NewRunner(cfg).Run()
	if !errors.Is(err, model.ErrInterrupted) {
		t.Fatalf("应触发受控中断，实际 %v", err)
	}
	store := checkpoint.NewStore(dir)
	st, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("中断后检查点应存在: %v ok=%v", err, ok)
	}
	if st.Stage != checkpoint.StageAnalyze {
		t.Fatalf("中断阶段应为 analyze，实际 %s", st.Stage)
	}

	// 续跑完成且产物与无中断运行一致。
	resume := cfg
	resume.InterruptAfter = 0
	rep, err := NewRunner(resume).Run()
	if err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	if rep.RecoveredSamples != 48000 || len(rep.SHA256) != 64 {
		t.Fatalf("续跑产物异常: %+v", rep)
	}

	// 对照：无中断的独立运行应得到相同 SHA。
	cleanDir := t.TempDir()
	clean := testConfig(t, cleanDir, 0)
	clean.JobID = cfg.JobID
	cleanRep, err := NewRunner(clean).Run()
	if err != nil {
		t.Fatal(err)
	}
	if cleanRep.SHA256 != rep.SHA256 {
		t.Fatalf("中断续跑产物 SHA 与连续运行不一致")
	}
}

func TestCheckpointRejectsSampleCountChange(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir, 0)
	if _, err := NewRunner(cfg).Run(); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.Samples = 24000
	_, err := NewRunner(changed).Run()
	if !errors.Is(err, model.ErrUnrecoverable) {
		t.Fatalf("采样数变化应拒绝续跑，实际 %v", err)
	}
}

func TestInvalidConfigExitCodeTwo(t *testing.T) {
	cases := []Config{
		{OutDir: t.TempDir(), Tracks: 0, Samples: 100, Bandwidth: []int64{1}},
		{OutDir: t.TempDir(), Tracks: 2, Samples: 0, Bandwidth: []int64{1}},
		{OutDir: t.TempDir(), Tracks: 2, Samples: 100, Bandwidth: nil},
		{OutDir: "", Tracks: 2, Samples: 100, Bandwidth: []int64{1}},
		{OutDir: t.TempDir(), Tracks: 2, Samples: model.MaxSamples + 1, Bandwidth: []int64{1}},
		{OutDir: t.TempDir(), Tracks: model.MaxTracks + 1, Samples: 100, Bandwidth: []int64{1}},
	}
	for i, cfg := range cases {
		cfg.JobID = "bad"
		cfg.Seed = 1
		_, err := NewRunner(cfg).Run()
		if model.ExitCode(err) != 2 {
			t.Fatalf("用例 %d 应映射退出码 2，实际 err=%v code=%d", i, err, model.ExitCode(err))
		}
	}
}

func TestAnchorNAWhenNoAnchors(t *testing.T) {
	clock, _ := model.NewClock(48000)
	tl, _ := model.NewTrackTimeline(0, clock, 1000)
	rep := analyze.AnalyzeAnchors(tl)
	if rep.Available {
		t.Fatal("无锚点时应输出 N/A（Available=false）")
	}
	tl.AddAnchor(model.Anchor{Sample: 0})
	if analyze.AnalyzeAnchors(tl).Available {
		t.Fatal("单锚点仍应 N/A")
	}
	tl.AddAnchor(model.Anchor{Sample: 500})
	if !analyze.AnalyzeAnchors(tl).Available {
		t.Fatal("两个锚点后应能计算间隔")
	}
}

func TestDamageAcrossTracksReflectedInManifest(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir, 0)
	rep, err := NewRunner(cfg).Run()
	if err != nil {
		t.Fatal(err)
	}
	// 跨轨缺口必须同时出现在分析报告与再封装清单中。
	trackWithGap := map[int]bool{}
	for _, dr := range rep.DamageReports {
		if len(dr.Segments) > 0 {
			trackWithGap[dr.TrackID] = true
		}
	}
	if len(trackWithGap) < 2 {
		t.Fatalf("故障应在多条轨道上产生缺口，实际 %v", trackWithGap)
	}
	manifestTracks := map[int]bool{}
	for _, e := range rep.Manifest.Entries {
		manifestTracks[e.TrackID] = true
		if e.Strategy != "zero-fill" || e.ZeroFilledSamples != e.EndSample-e.StartSample {
			t.Fatalf("清单条目补零记账错误: %+v", e)
		}
	}
	for id := range trackWithGap {
		if !manifestTracks[id] {
			t.Fatalf("轨道 %d 的缺口未反映到再封装清单", id)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("清单文件为空")
	}
}

func TestNoAnchorsReportsNA(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir, 0)
	cfg.NoAnchors = true
	cfg.Faults = FaultMask{} // 纯正常样例，仅验证锚点缺标。
	rep, err := NewRunner(cfg).Run()
	if err != nil {
		t.Fatalf("无锚点运行失败: %v", err)
	}
	if !rep.AnchorNA {
		t.Fatal("无锚点时 anchor_intervals 应为 N/A")
	}
}

func TestOutputsStayInsideOutDir(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir, 0)
	if _, err := NewRunner(cfg).Run(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"recovered.wav": false, "manifest.json": false, "checkpoint.json": false}
	for _, e := range entries {
		if _, ok := want[e.Name()]; ok {
			want[e.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("输出目录缺少 %s", name)
		}
	}
}
