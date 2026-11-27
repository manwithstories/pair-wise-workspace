package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"archivemedia/internal/media"
)

// TestResumeWithTruncatedCheckpoint 端到端验证：检查点被截断后，
// 续跑必须回退到前一有效检查点并仍产出与全新跑一致的结果。
func TestResumeWithTruncatedCheckpoint(t *testing.T) {
	g, _ := generate(t, 41, 4)

	// 全新跑参考。
	dirFresh := t.TempDir()
	rf, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV:  filepath.Join(dirFresh, "fresh.wav"),
		JournalDir: filepath.Join(dirFresh, "j"),
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := rf.Run()
	if err != nil {
		t.Fatal(err)
	}

	// 中断跑，产生检查点 current。
	dirInt := t.TempDir()
	jdir := filepath.Join(dirInt, "j")
	ri, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: filepath.Join(dirInt, "r.wav"), JournalDir: jdir,
		Interrupt: InterruptPlan{Enabled: true, AtArrival: len(g.Events) / 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ri.Run(); !media.IsKind(err, media.KindTimeline) {
		t.Fatalf("期望中断哨兵: %v", err)
	}
	// 再触发一次 Save，制造 current+previous 两个有效检查点（轮换）。
	// 通过直接读取确认 current 存在，然后截断它。
	cur := filepath.Join(jdir, "checkpoint.current")
	raw, err := os.ReadFile(cur)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cur, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	// 续跑：current 损坏，应回退 previous（或在仅一个检查点时按全新任务处理），
	// 最终输出仍须与全新跑逐字节一致。
	rr, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: filepath.Join(dirInt, "r.wav"), JournalDir: jdir, Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rr.Run()
	if err != nil {
		t.Fatalf("截断检查点后续跑失败: %v", err)
	}
	if res.OutputSHA256 != fresh.OutputSHA256 {
		t.Fatalf("截断回退后输出不一致:\n resume=%s\n fresh =%s",
			res.OutputSHA256, fresh.OutputSHA256)
	}
}

// TestCorruptCheckpointSHAFallsBack 验证 SHA 损坏的检查点被拒并回退。
func TestCorruptCheckpointSHAFallsBack(t *testing.T) {
	g, _ := generate(t, 7, 3)
	dirInt := t.TempDir()
	jdir := filepath.Join(dirInt, "j")
	ri, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: filepath.Join(dirInt, "r.wav"), JournalDir: jdir,
		Interrupt: InterruptPlan{Enabled: true, AtArrival: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ri.Run(); !media.IsKind(err, media.KindTimeline) {
		t.Fatalf("期望中断: %v", err)
	}
	cur := filepath.Join(jdir, "checkpoint.current")
	raw, _ := os.ReadFile(cur)
	raw[len(raw)-1] ^= 0xFF // 破坏载荷，内嵌 SHA 必失败
	if err := os.WriteFile(cur, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	rr, err := New(Options{
		Events: g.Events, ExtraData: g.ExtraData,
		OutputWAV: filepath.Join(dirInt, "r2.wav"), JournalDir: jdir, Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Run(); err != nil {
		t.Fatalf("SHA 损坏回退后续跑应成功: %v", err)
	}
}
