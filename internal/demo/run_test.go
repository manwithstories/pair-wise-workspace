package demo_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tsproof/internal/demo"
)

func TestRunAllAcceptanceMetrics(t *testing.T) {
	dir := t.TempDir()
	m, code := demo.Run(20261126, dir)
	if code != 0 {
		t.Fatalf("exit code=%d want 0, metrics=%+v", code, m)
	}
	if m.RoundtripConsistency != "100.00%" || m.RoundtripPct != 100.0 {
		t.Fatalf("roundtrip=%s", m.RoundtripConsistency)
	}
	if m.KeyframeHit != "100.00% (32/32)" {
		t.Fatalf("keyframe_hit=%s", m.KeyframeHit)
	}
	if m.Segments != 24 || m.HashMatch != "24/24" {
		t.Fatalf("segments=%d hash=%s", m.Segments, m.HashMatch)
	}
	if m.ReplayIdentical != "yes" {
		t.Fatalf("replay=%s", m.ReplayIdentical)
	}
	if m.ResumeSegments != 12 {
		t.Fatalf("resume_segments=%d want 12", m.ResumeSegments)
	}
	// 每类损坏都必须被观测到。
	if m.CCGaps == 0 || m.PSISwitches == 0 || m.CRCReject == 0 || m.Truncated == 0 ||
		m.Duplicates == 0 || m.Reorders == 0 || m.PCRWraps == 0 {
		t.Fatalf("damage coverage incomplete: %+v", m)
	}
}

func TestArtifactsWritten(t *testing.T) {
	dir := t.TempDir()
	if _, code := demo.Run(7, dir); code != 0 {
		t.Fatalf("code=%d", code)
	}
	// 32 帧 PNG。
	frames, err := os.ReadDir(filepath.Join(dir, "frames"))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 32 {
		t.Fatalf("frames=%d want 32", len(frames))
	}
	// metrics.json 可解析且字段齐全。
	raw, err := os.ReadFile(filepath.Join(dir, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m demo.Metrics
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Seed != 7 {
		t.Fatalf("persisted seed=%d", m.Seed)
	}
	// 检查点文件存在且记录 24 个提交片段。
	cp, err := os.ReadFile(filepath.Join(dir, "checkpoint", "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(cp, &snapshot); err != nil {
		t.Fatal(err)
	}
	committed, ok := snapshot["committed"].([]any)
	if !ok || len(committed) != 24 {
		t.Fatalf("committed segments=%v", snapshot["committed"])
	}
}

func TestReplayAcrossRunsIdentical(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	mA, cA := demo.Run(555, dirA)
	mB, cB := demo.Run(555, dirB)
	if cA != 0 || cB != 0 {
		t.Fatalf("codes=%d %d", cA, cB)
	}
	if mA.HashMatch != mB.HashMatch || mA.ReplayIdentical != "yes" {
		t.Fatalf("runs diverged: %+v vs %+v", mA, mB)
	}
	// 逐帧哈希清单必须一致。
	ma, _ := os.ReadFile(filepath.Join(dirA, "manifest.json"))
	mb, _ := os.ReadFile(filepath.Join(dirB, "manifest.json"))
	if string(ma) != string(mb) {
		t.Fatal("manifest not identical across replay")
	}
}
