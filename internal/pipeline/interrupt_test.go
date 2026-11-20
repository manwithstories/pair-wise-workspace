package pipeline

import (
	"context"
	"testing"

	"media-audit/internal/stream"
)

func TestInterruptResumeReusesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	cfg := DemoConfig{
		Codec: stream.AV1, Representations: 3, Frames: 1800,
		BlockFrames: 300, InterruptBlock: 2, Resume: true,
		CorruptFrame: -1, WorkDir: dir,
	}
	mt, m, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mt.ResumeMS < 0 || mt.ResumeMS > 3000 {
		t.Fatalf("恢复耗时=%dms 超出 3000ms 预算", mt.ResumeMS)
	}
	if m.SegmentCount == 0 {
		t.Fatalf("中断恢复后清单不应为空")
	}
}

func TestInterruptAndFullRunProduceSameHash(t *testing.T) {
	base := func(interrupt int) DemoConfig {
		return DemoConfig{
			Codec: stream.H264, Representations: 2, Frames: 1200,
			BlockFrames: 300, CorruptFrame: 600, WorkDir: t.TempDir(),
			InterruptBlock: interrupt, Resume: interrupt >= 0,
		}
	}
	_, mInterrupt, err := Run(context.Background(), base(1))
	if err != nil {
		t.Fatal(err)
	}
	_, mFull, err := Run(context.Background(), base(-1))
	if err != nil {
		t.Fatal(err)
	}
	if mInterrupt.SHA256 != mFull.SHA256 {
		t.Fatalf("中断恢复与完整运行的清单校验和不一致")
	}
}

func TestClampBlocks(t *testing.T) {
	blocks := []stream.BlockRecord{
		{Number: 0}, {Number: 1}, {Number: 2}, {Number: 3},
	}
	if got := clampBlocks(blocks, 2); len(got) != 2 || got[1].Number != 1 {
		t.Fatalf("截断块数错误: %+v", got)
	}
	if got := clampBlocks(blocks, 10); len(got) != 4 {
		t.Fatalf("n 超总数时应返回全部")
	}
	// 不修改原切片。
	if len(blocks) != 4 {
		t.Fatalf("clampBlocks 不得修改输入切片")
	}
}
