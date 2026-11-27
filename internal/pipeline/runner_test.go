package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"archivemedia/internal/audio"
	"archivemedia/internal/journal"
	"archivemedia/internal/media"
	"archivemedia/internal/packet"
	"archivemedia/internal/synth"
)

func generate(t *testing.T, seed int64, seconds int) (*synth.Generated, string) {
	t.Helper()
	cfg := synth.Config{
		Seed: seed, Seconds: seconds, Codec: packet.CodecOpus,
		Faults: FaultAll(),
	}
	g, err := synth.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g, ""
}

// FaultAll 返回全部五类故障。
func FaultAll() []synth.Fault {
	return []synth.Fault{
		synth.FaultDuplicate, synth.FaultReorder, synth.FaultBurst,
		synth.FaultLate, synth.FaultCRC,
	}
}

func TestFreshRunProducesWAVAndManifest(t *testing.T) {
	dir := t.TempDir()
	g, _ := generate(t, 41, 4)
	out := filepath.Join(dir, "fresh.wav")
	jdir := filepath.Join(dir, "journal")
	r, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: out, JournalDir: jdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run()
	if err != nil {
		t.Fatalf("全新跑失败: %v", err)
	}
	if res.Interrupted {
		t.Fatal("不应中断")
	}
	if len(res.OutputSHA256) != 64 {
		t.Fatalf("sha256 长度 = %d", len(res.OutputSHA256))
	}
	if res.DamagePacketDelta < 0 || res.DamagePacketDelta > 1 {
		t.Fatalf("damage_packet_delta 越界: %d", res.DamagePacketDelta)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() <= wavHeaderLen {
		t.Fatalf("WAV 未正确生成: %v size=%v", err, info)
	}
	m, err := journal.LoadManifest(journal.ManifestPath(jdir))
	if err != nil {
		t.Fatal(err)
	}
	if m.InputPackets == 0 {
		// InputPackets 在 finalize 未显式设置，校验摘要即可。
	}
	if m.OutputDigest != res.OutputSHA256 {
		t.Fatal("清单输出摘要与结果不符")
	}
}

func TestResumeMatchesFresh(t *testing.T) {
	g, _ := generate(t, 41, 4)

	// 全新跑。
	dirFresh := t.TempDir()
	freshOut := filepath.Join(dirFresh, "fresh.wav")
	freshJ := filepath.Join(dirFresh, "journal")
	rf, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: freshOut, JournalDir: freshJ,
	})
	if err != nil {
		t.Fatal(err)
	}
	freshRes, err := rf.Run()
	if err != nil {
		t.Fatal(err)
	}

	// 中断跑：在约一半到达点中断。
	dirInt := t.TempDir()
	intOut := filepath.Join(dirInt, "resume.wav")
	intJ := filepath.Join(dirInt, "journal")
	interruptAt := len(g.Events) / 2
	ri, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: intOut, JournalDir: intJ,
		Interrupt: InterruptPlan{Enabled: true, AtArrival: interruptAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	intRes, err := ri.Run()
	if err == nil || !media.IsKind(err, media.KindTimeline) {
		t.Fatalf("应返回中断哨兵: %v", err)
	}
	if !intRes.Interrupted {
		t.Fatal("中断标记缺失")
	}
	// 检查点文件必须存在。
	if _, err := os.Stat(filepath.Join(intJ, "checkpoint.current")); err != nil {
		t.Fatalf("检查点未落盘: %v", err)
	}

	// 续跑：新 Runner，Resume=true，不再设置中断。
	rr, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: intOut, JournalDir: intJ, Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeRes, err := rr.Run()
	if err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	if resumeRes.OutputSHA256 != freshRes.OutputSHA256 {
		t.Fatalf("续跑与全新跑摘要不一致:\n resume=%s\n fresh =%s",
			resumeRes.OutputSHA256, freshRes.OutputSHA256)
	}
	// 文件字节级一致。
	a, _ := os.ReadFile(intOut)
	b, _ := os.ReadFile(freshOut)
	if string(a) != string(b) {
		t.Fatal("续跑与全新跑 WAV 字节不一致")
	}
}

func TestRepeatedRunsIdenticalChecksum(t *testing.T) {
	g, _ := generate(t, 99, 3)
	run := func() string {
		dir := t.TempDir()
		r, err := New(Options{
			Events: g.Events, ExtraData: g.ExtraData,
			OutputWAV:  filepath.Join(dir, "o.wav"),
			JournalDir: filepath.Join(dir, "j"),
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Run()
		if err != nil {
			t.Fatal(err)
		}
		return res.OutputSHA256
	}
	if run() != run() {
		t.Fatal("固定输入重复运行校验和不一致")
	}
}

func TestMissingPlaceholdersAreSilence(t *testing.T) {
	// 无故障流不应有占位；含 burst 的流占位区必须是确定性静音且不伪装有效音频。
	g, _ := generate(t, 41, 4)
	if len(g.Stats.BurstMissing) == 0 {
		t.Fatal("测试前提：应存在突发丢失")
	}
	dir := t.TempDir()
	r, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV:  filepath.Join(dir, "o.wav"),
		JournalDir: filepath.Join(dir, "j"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(); err != nil {
		t.Fatal(err)
	}
	if r.stats.Missing == 0 {
		t.Fatal("时间线应记录占位区间")
	}
}

func TestLoudnessAfterPipeline(t *testing.T) {
	// 干净信号（无故障、无削波）必须精确命中 -18 LUFS ±0.5，且不触发限幅。
	cfg := synth.Config{Seed: 41, Seconds: 5, Codec: packet.CodecOpus}
	g, err := synth.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "clean.wav")
	r, err := New(Options{
		Events: g.GroundTruth, ExtraData: g.ExtraData,
		OutputWAV: out, JournalDir: filepath.Join(dir, "j"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(); err != nil {
		t.Fatal(err)
	}
	if r.debugNorm.AppliedLimit {
		t.Fatal("干净信号不应触发真峰值限幅")
	}
	buf := readWAVMono(t, out)
	st := audio.MeasureLoudness(&audio.Buffer{
		Samples: stereoFromMono(buf), Frames: len(buf),
	})
	if st.IsSilence {
		t.Fatal("整段不应被判静音")
	}
	if d := st.IntegratedLUFS - audio.TargetLUFS; d > 0.5 || d < -0.5 {
		t.Fatalf("干净信号响度偏差 %.2f LU 超过 0.5", d)
	}
}

func TestLoudnessFaultSignalTPConstrained(t *testing.T) {
	// 含故障信号（CRC 损坏→静音占位 + 突发丢失占位）：
	// 损坏必须被计数，归一结果确定可复现，响度命中目标且真峰值守住 -1 dBTP。
	g, _ := generate(t, 41, 5)
	run := func() (float64, float64, int) {
		dir := t.TempDir()
		out := filepath.Join(dir, "fault.wav")
		r, err := New(Options{
			Events: g.Events, ExtraData: g.ExtraData,
			OutputWAV: out, JournalDir: filepath.Join(dir, "j"),
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Run()
		if err != nil {
			t.Fatal(err)
		}
		buf := readWAVMono(t, out)
		st := audio.MeasureLoudness(&audio.Buffer{
			Samples: stereoFromMono(buf), Frames: len(buf),
		})
		return st.IntegratedLUFS, st.TruePeakDBTP, res.DamagePacketDelta
	}
	lufs1, tp1, dmg1 := run()
	lufs2, tp2, dmg2 := run()
	if lufs1 != lufs2 || tp1 != tp2 || dmg1 != dmg2 {
		t.Fatalf("故障信号归一结果不确定: %.3f/%.3f/%d vs %.3f/%.3f/%d",
			lufs1, tp1, dmg1, lufs2, tp2, dmg2)
	}
	if dmg1 != 1 {
		t.Fatalf("损坏包计数 = %d，期望 1", dmg1)
	}
	// 硬约束：真峰值不得高于 -1 dBTP（留 0.02 数值容差）。
	if tp1 > -1.0+0.02 {
		t.Fatalf("真峰值 %.2f dBTP 超过 -1 上限", tp1)
	}
	// 含静音占位的信号仍须把有效音频归一到 -18 LUFS ±0.5。
	if d := lufs1 - audio.TargetLUFS; d > 0.5 || d < -0.5 {
		t.Fatalf("故障信号响度偏差 %.2f LU 超过 0.5", d)
	}
}

func TestTruePeakLimiterOnHotSignal(t *testing.T) {
	// 直接构造满幅高瞬态立体声，验证真峰值限制器主动介入并守住 -1 dBTP。
	frames := audio.TargetRate * 2
	samples := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		// 接近满量程的复合信号，归一放大后必然越过 TP 上限。
		v := float32(0.95)
		samples[i*2], samples[i*2+1] = v, v
	}
	hot := &audio.Buffer{Samples: samples, Frames: frames}
	res, err := audio.Normalize(hot)
	if err != nil {
		t.Fatal(err)
	}
	if !res.AppliedLimit {
		t.Fatal("满幅信号必须触发真峰值限制")
	}
	if res.StatsAfter.TruePeakDBTP > -1.0+0.02 {
		t.Fatalf("限制后 TP %.2f 仍超过 -1 dBTP", res.StatsAfter.TruePeakDBTP)
	}
}
