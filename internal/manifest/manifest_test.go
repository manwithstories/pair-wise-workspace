package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/quality"
	"medpipe/internal/timeline"
)

func sampleManifest(t *testing.T, dir string, maxErr int64) *Manifest {
	framePath := filepath.Join(dir, "frame_000000.jpg")
	jpeg := []byte("jpeg-bytes-0")
	if err := os.WriteFile(framePath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	fps, _ := clock.NewRational(10, 1)
	sum := quality.Summary{
		Frames: 1, TargetBPS: 1000, ActualBPS: 1100, DeviationPct: 10,
		PSNRMin: 40, PSNRMean: 40, MaxErrorSamples: maxErr,
		FrameSHA256: quality.ChainHash([]string{quality.HashBytes(jpeg)}),
	}
	frames := []quality.FrameMetric{{
		Index: 0, File: "frame_000000.jpg", SHA256: quality.HashBytes(jpeg),
		Width: 320, Height: 240, Bytes: len(jpeg), PSNR: 40,
	}}
	sources := []timeline.SourceRange{{StartFrame: 0, EndFrame: 10, BlockID: 0, Source: "source"}}
	blocks := []BlockRecord{{ID: 0, Frames: 10, Compensated: 0}}
	return Build(41, 48000, 2, fps, "", frames, sum, sources, blocks, "ckpt-digest")
}

func TestWriteLoadVerify(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest(t, dir, 0)
	path, err := m.Write(dir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Schema != "medpipe/manifest/v1" || got.Seed != 41 || got.FPS != "10/1" {
		t.Fatalf("清单字段错误: %+v", got)
	}
	if err := got.Verify(dir); err != nil {
		t.Fatalf("Verify 应通过: %v", err)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest(t, dir, 0)
	if _, err := m.Write(dir); err != nil {
		t.Fatal(err)
	}
	// 篡改帧文件后哈希必须失配
	raw, _ := os.ReadFile(filepath.Join(dir, "frame_000000.jpg"))
	raw[0] ^= 0xFF
	os.WriteFile(filepath.Join(dir, "frame_000000.jpg"), raw, 0o644)
	loaded, _ := Load(filepath.Join(dir, "manifest.json"))
	if err := loaded.Verify(dir); clock.CodeOf(err) != clock.CodeChecksum {
		t.Fatalf("篡改应报 checksum, 得 %v", err)
	}
}

func TestRejectManifestOnLargeAlignError(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest(t, dir, 2) // 超过 1 采样容忍度
	if _, err := m.Write(dir); clock.CodeOf(err) != clock.CodeInvalidHeader {
		t.Fatalf("对齐误差超限应拒绝清单, 得 %v", err)
	}
	if _, err := (*Manifest)(nil).Write(dir); clock.CodeOf(err) != clock.CodeConflict {
		t.Fatalf("空结果应按 conflict 拒绝, 得 %v", err)
	}
}

func TestBlockRecordCompensation(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest(t, dir, 0)
	m.Blocks = append(m.Blocks, BlockRecord{
		ID: 1, Truncated: true, MissingFrames: 4, Compensated: 8,
	})
	if _, err := m.Write(dir); err != nil {
		t.Fatal(err)
	}
	loaded, _ := Load(filepath.Join(dir, "manifest.json"))
	if len(loaded.Blocks) != 2 || loaded.Blocks[1].Compensated != 8 || !loaded.Blocks[1].Truncated {
		t.Fatalf("截断补偿记录丢失: %+v", loaded.Blocks)
	}
}
