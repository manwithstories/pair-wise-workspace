package engine

import (
	"os"
	"path/filepath"
	"testing"

	"relay/internal/index"
	"relay/internal/media"
	"relay/internal/streamgen"
)

// TestFramingDamageReturnsDamaged 验证分帧级损坏（无法定位下一条边界）
// 向上传播为可识别的损坏输入错误，由 CLI 映射到退出码 2。
func TestFramingDamageReturnsDamaged(t *testing.T) {
	dir := t.TempDir()
	stream, _ := streamgen.Generate(streamgen.Spec{
		Seed: 1, DurationMS: 15000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, PPM: 0, DamageSeq: 1200,
		DamageKind: streamgen.DamageTruncated,
	})
	in := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(in, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		InputPath: in, OutputPath: filepath.Join(dir, "out.pcm"),
		WorkDir: filepath.Join(dir, "work"), Params: params(),
	}
	_, err := Run(cfg)
	if err == nil {
		t.Fatal("framing damage must abort with an error")
	}
	if !IsDamaged(err) {
		t.Fatalf("want damaged-input error, got %v", err)
	}
	if f, ok := media.FindFault(err); !ok || f.Kind != media.FaultFraming {
		t.Fatalf("underlying cause must be FaultFraming, got %v", err)
	}
}

// TestUnsupportedParamsExitThree 验证不支持的编码参数返回 UnsupportedError。
func TestUnsupportedParamsExitThree(t *testing.T) {
	dir := t.TempDir()
	stream, _ := streamgen.Generate(streamgen.Spec{
		Seed: 1, DurationMS: 1000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, DamageSeq: 0xFFFF,
	})
	in := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(in, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	bad := index.Params{
		AudioRate: 44100, Channels: 2, PayloadType: 97, // 44.1k 不支持
		ClockRate: 44100, TargetRate: 48000, TargetChans: 2,
	}
	cfg := Config{
		InputPath: in, OutputPath: filepath.Join(dir, "out.pcm"),
		WorkDir: filepath.Join(dir, "work"), Params: bad,
	}
	_, err := Run(cfg)
	if _, ok := err.(*media.UnsupportedError); !ok {
		t.Fatalf("want UnsupportedError, got %v", err)
	}
}
