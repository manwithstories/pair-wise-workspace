package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediakernel/internal/ebml"
)

func testOptions(dir string, resume bool) Options {
	return Options{
		WorkDir: dir, Seed: 17, LossBurst: 12,
		BandwidthKbps: 420, Resume: resume,
	}
}

func TestRunMetrics(t *testing.T) {
	res, err := Run(context.Background(), testOptions(t.TempDir(), false))
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics
	if m.SourceSHA256 == "" || m.Resumed || m.CacheRejected {
		t.Fatalf("metrics: %+v", m)
	}
	if m.Recovered == 0 {
		t.Fatal("expected retransmission recoveries")
	}
	if m.Lost == 0 {
		t.Fatal("expected unrecoverable gaps")
	}
	if m.TimelineErrMs > 20 {
		t.Fatalf("timeline error %dms over 20ms", m.TimelineErrMs)
	}
	if m.BitrateErrPct > 8 {
		t.Fatalf("bitrate error %.2f%% over 8%%", m.BitrateErrPct)
	}
	if m.ManifestStartMs != 0 || m.TargetKbps != 420 {
		t.Fatalf("metrics: %+v", m)
	}
	// 空洞记录带原因
	if len(res.Metrics.SourceSHA256) != 64 {
		t.Fatalf("bad digest: %q", res.Metrics.SourceSHA256)
	}
}

func TestRunDeterministicAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	r1, err := Run(context.Background(), testOptions(dir, false))
	if err != nil {
		t.Fatal(err)
	}
	// 重复运行：结果一致，包不重复计入
	r2, err := Run(context.Background(), testOptions(dir, false))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Metrics != r2.Metrics {
		t.Fatalf("rerun mismatch:\n%+v\n%+v", r1.Metrics, r2.Metrics)
	}
	// 续跑：从检查点恢复，结果一致
	r3, err := Run(context.Background(), testOptions(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	if !r3.Metrics.Resumed {
		t.Fatal("expected checkpoint_resumed")
	}
	r3.Metrics.Resumed = false
	if r1.Metrics != r3.Metrics {
		t.Fatalf("resume mismatch:\n%+v\n%+v", r1.Metrics, r3.Metrics)
	}
}

func TestTruncatedInputReportsOffset(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(context.Background(), testOptions(dir, false)); err != nil {
		t.Fatal(err)
	}
	// 截断输入
	path := filepath.Join(dir, "input.webm")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), testOptions(dir, false))
	var te *ebml.TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("want TruncatedError, got %v", err)
	}
	if te.Offset <= 0 {
		t.Fatalf("offset=%d", te.Offset)
	}
}

func TestCacheTamperQuarantinedAndRecomputed(t *testing.T) {
	dir := t.TempDir()
	r1, err := Run(context.Background(), testOptions(dir, false))
	if err != nil {
		t.Fatal(err)
	}
	// 篡改一个缓存对象
	entries, err := os.ReadDir(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var victim string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == "" {
			victim = filepath.Join(dir, "cache", e.Name())
			break
		}
	}
	if victim == "" {
		t.Fatal("no cache object found")
	}
	if err := os.WriteFile(victim, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2, err := Run(context.Background(), testOptions(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Metrics.CacheRejected {
		t.Fatal("expected cache_rejected")
	}
	// 重算结果与首次一致
	r2.Metrics.CacheRejected = false
	r2.Metrics.Resumed = false
	if r1.Metrics != r2.Metrics {
		t.Fatalf("recompute mismatch:\n%+v\n%+v", r1.Metrics, r2.Metrics)
	}
	// 被篡改对象已隔离
	if _, err := os.Stat(filepath.Join(dir, "cache", "quarantine", filepath.Base(victim))); err != nil {
		t.Fatal("tampered object not quarantined")
	}
}

func TestCorruptCheckpointRecomputed(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(context.Background(), testOptions(dir, false)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), testOptions(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Metrics.CacheRejected {
		t.Fatal("expected cache_rejected for corrupt checkpoint")
	}
}

func TestCancelPreservesState(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	if _, err := Run(ctx, testOptions(dir, false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// 重跑可完成
	if _, err := Run(context.Background(), testOptions(dir, false)); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointSavedPerStage(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(context.Background(), testOptions(dir, false)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty checkpoint")
	}
	// 清单已落盘
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal("manifest not written")
	}
}

func TestRunTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := Run(ctx, testOptions(t.TempDir(), false)); err != nil {
		t.Fatal(err)
	}
}
