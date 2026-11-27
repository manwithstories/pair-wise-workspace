package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relay/internal/index"
	"relay/internal/streamgen"
)

func params() index.Params {
	return index.Params{
		AudioRate: 48000, Channels: 2, PayloadType: 97,
		ClockRate: 48000, TargetRate: 48000, TargetChans: 2,
	}
}

func writeFixture(t *testing.T, dir string, spec streamgen.Spec) string {
	t.Helper()
	stream, _ := streamgen.Generate(spec)
	p := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(p, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256File(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestRunProduces48kStereoAndChecksumStable(t *testing.T) {
	dir := t.TempDir()
	in := writeFixture(t, dir, streamgen.Spec{
		Seed: 41, DurationMS: 3000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, PPM: 0, DamageSeq: 0xFFFF,
	})
	cfg := Config{
		InputPath: in, OutputPath: filepath.Join(dir, "out.pcm"),
		WorkDir: filepath.Join(dir, "work"), Params: params(),
	}
	rep, err := Run(cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.OutRate != 48000 || rep.Channels != 2 {
		t.Fatalf("format=%d/%d", rep.OutRate, rep.Channels)
	}
	first := sha256File(t, cfg.OutputPath)
	if len(first) != 64 {
		t.Fatalf("bad checksum %q", first)
	}
	// 清掉检查点与索引后重跑，产物必须字节一致。
	os.RemoveAll(cfg.WorkDir)
	rep2, err := Run(cfg)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	second := sha256File(t, cfg.OutputPath)
	if first != second {
		t.Fatalf("checksum drift: %s vs %s", first, second)
	}
	rep.ChecksumA, rep2.ChecksumB = first, second
	rep.ChecksumMatch = rep.ChecksumA == rep2.ChecksumB
	if !rep.ChecksumMatch {
		t.Fatal("checksum match flag false")
	}
}

func TestResumeAfterInterrupt(t *testing.T) {
	dir := t.TempDir()
	in := writeFixture(t, dir, streamgen.Spec{
		Seed: 7, DurationMS: 8000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, PPM: 150, DamageSeq: 0xFFFF,
	})
	cfg := Config{
		InputPath: in, OutputPath: filepath.Join(dir, "out.pcm"),
		WorkDir: filepath.Join(dir, "work"), Params: params(),
		InterruptAt: 50 * time.Millisecond,
	}
	// 第一次运行应在中断点退出。
	if _, err := Run(cfg); err == nil {
		t.Fatal("expected interrupt error")
	}
	cpPath := filepath.Join(cfg.WorkDir, "checkpoint.json")
	if _, err := os.Stat(cpPath); err != nil {
		t.Fatalf("checkpoint not persisted: %v", err)
	}
	// 不带中断续跑，应从检查点继续并完成。
	cfg.InterruptAt = 0
	start := time.Now()
	if _, err := Run(cfg); err != nil {
		t.Fatalf("resume: %v", err)
	}
	rep, err := Run(Config{
		InputPath: in, OutputPath: filepath.Join(dir, "ref.pcm"),
		WorkDir: filepath.Join(dir, "work-ref"), Params: params(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeMS := time.Since(start).Milliseconds()
	if resumeMS > 2000 {
		t.Fatalf("resume took %dms > 2000", resumeMS)
	}
	// 续跑产物必须与一次完整运行的参考产物一致。
	if a, b := sha256File(t, cfg.OutputPath), sha256File(t, filepath.Join(dir, "ref.pcm")); a != b {
		t.Fatalf("resume output differs: %s vs %s", a, b)
	}
	_ = rep
}

func TestResumeAfterWriteFailure(t *testing.T) {
	dir := t.TempDir()
	in := writeFixture(t, dir, streamgen.Spec{
		Seed: 9, DurationMS: 4000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, PPM: 0, DamageSeq: 0xFFFF,
	})
	cfg := Config{
		InputPath: in, OutputPath: filepath.Join(dir, "out.pcm"),
		WorkDir: filepath.Join(dir, "work"), Params: params(),
		InjectWriteFailAfter: 3,
	}
	// 第一次运行在第 3 次提交后注入写盘失败。
	if _, err := Run(cfg); err == nil {
		t.Fatal("expected injected write failure")
	}
	// 关闭注入后续跑完成。
	cfg.InjectWriteFailAfter = 0
	if _, err := Run(cfg); err != nil {
		t.Fatalf("resume after write fail: %v", err)
	}
	ref, err := Run(Config{
		InputPath: in, OutputPath: filepath.Join(dir, "ref.pcm"),
		WorkDir: filepath.Join(dir, "work-ref"), Params: params(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := sha256File(t, cfg.OutputPath), sha256File(t, filepath.Join(dir, "ref.pcm")); a != b {
		t.Fatalf("post-fail output differs: %s vs %s", a, b)
	}
	_ = ref
}

func TestBadPayloadLocatedWithinOnePacket(t *testing.T) {
	dir := t.TempDir()
	const damage uint16 = 12000
	in := writeFixture(t, dir, streamgen.Spec{
		Seed: 41, DurationMS: 130000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, PPM: 150, DamageSeq: damage,
		DamageKind: streamgen.DamageBadPayload,
	})
	cfg := Config{
		InputPath: in, OutputPath: filepath.Join(dir, "out.pcm"),
		WorkDir: filepath.Join(dir, "work"), Params: params(),
	}
	rep, err := Run(cfg)
	if err != nil {
		t.Fatalf("recoverable bad payload must not abort: %v", err)
	}
	rep.DamagePacket = int64(damage)
	if rep.LocatedPacket < 0 {
		t.Fatal("bad packet not located")
	}
	delta := rep.LocatedPacket - rep.DamagePacket
	if delta < 0 {
		delta = -delta
	}
	rep.LocateDelta = delta
	if delta > 1 {
		t.Fatalf("locate delta=%d > 1 packet", delta)
	}
}
