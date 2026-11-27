package fixture

import (
	"os"
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/index"
	"medpipe/internal/ingest"
	"medpipe/internal/quality"
)

func defaultSpec() Spec {
	return Spec{Seed: 41, Tracks: 3, Seconds: 2, SampleRate: 48000, FPS: 10, BlockFrames: 4800, Bits: 16}
}

func TestGenerateDeterministic(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	p1, err := Generate(dir1, defaultSpec())
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Generate(dir2, defaultSpec())
	if err != nil {
		t.Fatal(err)
	}
	if p1.BlockCount != 20 || p1.TotalFrames != 96000 || len(p1.Paths) != 20 {
		t.Fatalf("生成计划错误: count=%d total=%d paths=%d", p1.BlockCount, p1.TotalFrames, len(p1.Paths))
	}
	// 同种子两次生成的块必须逐字节一致
	for i := range p1.Paths {
		b1, _ := os.ReadFile(p1.Paths[i])
		b2, _ := os.ReadFile(p2.Paths[i])
		if len(b1) != len(b2) || quality.HashBytes(b1) != quality.HashBytes(b2) {
			t.Fatalf("块 %d 同种子内容不一致", i)
		}
	}
}

func TestGeneratedWAVScansClean(t *testing.T) {
	dir := t.TempDir()
	plan, err := Generate(dir, defaultSpec())
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.ScanDir(dir, index.DefaultMaxBlock)
	if err != nil {
		t.Fatalf("生成样例应能被扫描: %v", err)
	}
	if len(idx.Blocks) != plan.BlockCount {
		t.Fatalf("扫描块数 %d != 生成 %d", len(idx.Blocks), plan.BlockCount)
	}
	for _, b := range idx.Blocks {
		if b.SampleRate != 48000 || b.Channels != 3 || b.BitsPerSample != 16 || b.Truncated {
			t.Fatalf("块几何异常: %+v", b)
		}
	}
}

func TestCorruptTruncateAndHalfFrame(t *testing.T) {
	dir := t.TempDir()
	plan, _ := Generate(dir, defaultSpec())

	// 完整采样边界截断：扫描通过但标记 truncated 且有缺失帧
	id, err := Corrupt(dir, plan, ingest.FaultTruncate)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.ScanDir(dir, index.DefaultMaxBlock)
	if err != nil {
		t.Fatalf("整帧截断不应拒绝: %v", err)
	}
	var hit index.BlockIndex
	for _, b := range idx.Blocks {
		if b.ID == id {
			hit = b
		}
	}
	if !hit.Truncated || hit.MissingFrames <= 0 {
		t.Fatalf("截断块统计错误: %+v", hit)
	}

	// 半帧截断：必须被扫描拒绝
	dir2 := t.TempDir()
	plan2, _ := Generate(dir2, defaultSpec())
	id2, err := Corrupt(dir2, plan2, ingest.FaultHalfFrame)
	if err != nil {
		t.Fatal(err)
	}
	_, scanErr := index.ScanDir(dir2, index.DefaultMaxBlock)
	if clock.CodeOf(scanErr) != clock.CodeInvalidHeader {
		t.Fatalf("半帧应报 invalid_header, 得 %v", scanErr)
	}
	if clock.BlockOf(scanErr) != id2 {
		t.Fatalf("错误应携带块号 %d, 得 %d", id2, clock.BlockOf(scanErr))
	}
}

func TestCorruptBadHeader(t *testing.T) {
	dir := t.TempDir()
	plan, _ := Generate(dir, defaultSpec())
	id, err := Corrupt(dir, plan, ingest.FaultBadHeader)
	if err != nil || id != 0 {
		t.Fatalf("bad-header: id=%d err=%v", id, err)
	}
	if _, err := index.ScanDir(dir, index.DefaultMaxBlock); clock.CodeOf(err) != clock.CodeInvalidHeader {
		t.Fatalf("坏头应报 invalid_header, 得 %v", err)
	}
}

func TestUnknownFaultIsNoop(t *testing.T) {
	dir := t.TempDir()
	plan, _ := Generate(dir, defaultSpec())
	id, err := Corrupt(dir, plan, ingest.FaultNone)
	if err != nil || id != -1 {
		t.Fatalf("无故障应为 noop: id=%d err=%v", id, err)
	}
}
