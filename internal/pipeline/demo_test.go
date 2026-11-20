package pipeline

import (
	"context"
	"testing"

	"media-audit/internal/stream"
)

func TestRunEndToEndSmallScale(t *testing.T) {
	cfg := DemoConfig{
		Codec:           stream.AV1,
		Representations: 3,
		Frames:          1800,
		BlockFrames:     600,
		CorruptFrame:    900,
		Resume:          true,
		WorkDir:         t.TempDir(),
	}
	mt, _, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mt.Segments < 3 || mt.Segments > 30 {
		t.Fatalf("分片数=%d 超出小样本预期范围", mt.Segments)
	}
	if mt.AlignmentMaxMS > 40 {
		t.Fatalf("对齐偏差=%dms 超过 40ms", mt.AlignmentMaxMS)
	}
	if mt.LocationError < 0 || mt.LocationError > 4 {
		t.Fatalf("定位误差=%d, 期望 0-4 字节", mt.LocationError)
	}
	if len(mt.ManifestSHA256) != 64 {
		t.Fatalf("清单校验和格式错误")
	}
	if mt.ExitCode != 2 {
		t.Fatalf("精确定位损坏应退出码 2, got %d", mt.ExitCode)
	}
}

func TestRunDeterministicManifestHash(t *testing.T) {
	cfg := func() DemoConfig {
		return DemoConfig{
			Codec: stream.H264, Representations: 3, Frames: 1200,
			BlockFrames: 300, CorruptFrame: 500, WorkDir: t.TempDir(),
		}
	}
	mt1, m1, err := Run(context.Background(), cfg())
	if err != nil {
		t.Fatal(err)
	}
	mt2, m2, err := Run(context.Background(), cfg())
	if err != nil {
		t.Fatal(err)
	}
	if m1.SHA256 != m2.SHA256 {
		t.Fatalf("两次运行清单校验和不一致")
	}
	if mt1.ManifestSHA256 != mt2.ManifestSHA256 {
		t.Fatalf("指标中的校验和不确定")
	}
}

func TestRunNoCorruptionExitZero(t *testing.T) {
	cfg := DemoConfig{
		Codec: stream.H265, Representations: 2, Frames: 900,
		BlockFrames: 300, CorruptFrame: -1, WorkDir: t.TempDir(),
	}
	mt, _, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mt.ExitCode != 0 || mt.CorruptOffset != 0 || mt.LocationError != 0 {
		t.Fatalf("无损坏场景指标错误: %+v", mt)
	}
}

func TestRunAllCodecs(t *testing.T) {
	for _, codec := range []stream.Codec{stream.H264, stream.H265, stream.AV1} {
		cfg := DemoConfig{
			Codec: codec, Representations: 2, Frames: 600,
			BlockFrames: 200, CorruptFrame: 300, WorkDir: t.TempDir(),
		}
		mt, _, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatalf("%s 端到端失败: %v", codec, err)
		}
		if mt.LocationError > 4 {
			t.Fatalf("%s 定位误差=%d", codec, mt.LocationError)
		}
	}
}
