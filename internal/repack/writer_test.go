package repack

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"bcastqc/internal/ingest"
	"bcastqc/internal/model"
)

func TestWriterProducesValidWAVAndManifest(t *testing.T) {
	dir := t.TempDir()
	const frames, chans = 1000, 2
	w, err := NewWriter(dir, 48000, 16, chans, frames)
	if err != nil {
		t.Fatal(err)
	}
	// 写入 0..499 完好帧。
	good := make([]int, 500*chans)
	for i := range good {
		good[i] = i % 1000
	}
	if err := w.WriteFrames(good); err != nil {
		t.Fatal(err)
	}
	// 500..999 为缺口：只登记清单，主流写零。
	if err := w.RecordZeroFill(0, 500, frames, model.GapKindLoss, 0); err != nil {
		t.Fatal(err)
	}
	zero := make([]int, 500*chans)
	if err := w.WriteFrames(zero); err != nil {
		t.Fatal(err)
	}
	man, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if man.RecoveredSamples != frames {
		t.Fatalf("恢复采样数应为 %d，实际 %d", frames, man.RecoveredSamples)
	}
	if len(man.Entries) != 1 || man.Entries[0].ZeroFilledSamples != 500 {
		t.Fatalf("清单补零记账错误: %+v", man.Entries)
	}
	if len(man.SHA256) != 64 {
		t.Fatalf("SHA256 长度应为 64，实际 %d", len(man.SHA256))
	}

	// 独立重算文件 SHA，必须与清单一致。
	f, err := os.Open(filepath.Join(dir, "recovered.wav"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != man.SHA256 {
		t.Fatalf("清单 SHA %s 与文件实算 %s 不一致", man.SHA256, got)
	}

	// 产物必须能被 ingest 重新核验，且帧数正确。
	info, err := ingest.ParseWAVFile(filepath.Join(dir, "recovered.wav"))
	if err != nil {
		t.Fatalf("母版无法通过 WAV 核验: %v", err)
	}
	if info.TotalFrames != frames || info.NumChans != chans {
		t.Fatalf("母版帧数/声道错误: %+v", info)
	}
}

func TestWriterDeterministicOutput(t *testing.T) {
	build := func(dir string) string {
		w, _ := NewWriter(dir, 48000, 16, 1, 500)
		data := make([]int, 500)
		for i := range data {
			data[i] = (i * 7) % 300
		}
		_ = w.WriteFrames(data)
		man, err := w.Close()
		if err != nil {
			t.Fatal(err)
		}
		return man.SHA256
	}
	sha1 := build(t.TempDir())
	sha2 := build(t.TempDir())
	if sha1 != sha2 {
		t.Fatal("相同输入两次写出的 SHA 不一致")
	}
}

func TestRejectsMisalignedData(t *testing.T) {
	w, err := NewWriter(t.TempDir(), 48000, 16, 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrames([]int{1, 2, 3}); err == nil {
		t.Fatal("非声道整数倍的数据应被拒绝")
	}
}

func TestGapsToEntries(t *testing.T) {
	gaps := []model.Gap{
		{TrackID: 2, Start: 10, End: 20, Kind: model.GapKindCRC, LocError: 1},
	}
	entries := GapsToEntries(gaps)
	if len(entries) != 1 || entries[0].ZeroFilledSamples != 10 || entries[0].Strategy != "zero-fill" {
		t.Fatalf("缺口转清单条目错误: %+v", entries)
	}
}
