package resume

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/fault"
	"bcast/internal/model"
)

func makeSamples(n int, segUS int64) []model.Sample {
	out := make([]model.Sample, n)
	for i := 0; i < n; i++ {
		out[i] = model.Sample{
			TrackID: uint8(i % 5), Kind: model.KindVideo, Seq: uint64(i),
			PTS: int64(i) * 1000, Duration: 1000,
		}
	}
	return out
}

func makeGaps() []model.Gap {
	return []model.Gap{{TrackID: 0, AfterSeq: 3, BeforeSeq: 5, StartPTS: 4000, EndPTS: 5000}}
}

func manifest(n int) Manifest {
	return Manifest{SegmentUS: 5000, SampleCount: n, GapCount: 1, FaultCount: 2,
		SourceSHA256: "deadbeef" + repeat("0", 56)}
}

func writeFull(t *testing.T, dir, name, src string, cut int64) (*Journal, string) {
	t.Helper()
	samples := makeSamples(40, 5000)
	gaps := makeGaps()
	mpk := filepath.Join(dir, name)
	jPath := filepath.Join(dir, name+".journal.json")
	j, err := Load(jPath)
	if errors.Is(err, ErrNoJournal) {
		j = nil
	} else if err != nil {
		t.Fatal(err)
	}
	w, err := NewFullWriter(mpk, jPath, manifest(40).SourceSHA256, 5000, j)
	if err != nil {
		t.Fatal(err)
	}
	if cut > 0 {
		w.SetCutPoint(cut)
	}
	err = w.WriteAll(samples, gaps, nil, manifest(40))
	if cut > 0 && !errors.Is(err, ErrCut) {
		t.Fatalf("want ErrCut, got %v", err)
	}
	if cut == 0 && err != nil {
		t.Fatalf("full write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	jl, err := Load(jPath)
	if err != nil {
		t.Fatal(err)
	}
	return jl, mpk
}

func TestFullAndResumedAreByteIdentical(t *testing.T) {
	dir := t.TempDir()
	// 完整运行。
	_, fullPath := writeFull(t, filepath.Join(dir, "full"), "full.mpk", "src", 0)
	// 中断运行：在 12000us（第 3 段中段）切断。
	jl, cutPath := writeFull(t, filepath.Join(dir, "cut"), "resumed.mpk", "src", 12000)
	if jl.LastCommitted() < 1 {
		t.Fatalf("cut run should have committed some segments, got %d", jl.LastCommitted())
	}
	// 续跑：同一输出与检查点，无中断点，重放剩余未提交段。
	samples := makeSamples(40, 5000)
	w, err := NewFullWriter(cutPath, cutPath+".journal.json",
		manifest(40).SourceSHA256, 5000, jl)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAll(samples, makeGaps(), nil, manifest(40)); err != nil {
		t.Fatalf("resume write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(fullPath)
	resumed, _ := os.ReadFile(cutPath)
	if len(full) == 0 || string(full) != string(resumed) {
		t.Fatalf("full (%d bytes) and resumed (%d bytes) differ", len(full), len(resumed))
	}
	// 续跑后块校验必须通过。
	finalJ, err := Load(cutPath + ".journal.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBlocks(cutPath, finalJ); err != nil {
		t.Fatalf("VerifyBlocks: %v", err)
	}
}

func TestVerifyBlocksDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	jl, path := writeFull(t, dir, "x.mpk", "src", 0)
	data, _ := os.ReadFile(path)
	// 翻转某块体内的一个字节。
	data[MPKHeader+BlockHdr+3] ^= 0xFF
	tampered := filepath.Join(dir, "tampered.mpk")
	if err := os.WriteFile(tampered, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err := VerifyBlocks(tampered, jl)
	if !errors.Is(err, fault.ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
}

func TestResumeRejectsForeignSource(t *testing.T) {
	dir := t.TempDir()
	jl, _ := writeFull(t, dir, "x.mpk", "src", 0)
	other := Manifest{SourceSHA256: "ffff" + repeat("0", 60)}
	_, err := NewFullWriter(filepath.Join(dir, "y.mpk"),
		filepath.Join(dir, "y.journal"), other.SourceSHA256, 5000, jl)
	if !errors.Is(err, fault.ErrChecksum) {
		t.Fatalf("want ErrChecksum for foreign source, got %v", err)
	}
}

func TestFileSHA256Stable(t *testing.T) {
	dir := t.TempDir()
	_, p1 := writeFull(t, dir, "a.mpk", "src", 0)
	_, p2 := writeFull(t, filepath.Join(dir, "dup"), "a.mpk", "src", 0)
	h1, err := FileSHA256(p1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := FileSHA256(p2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("deterministic file hashes differ: %s vs %s", h1, h2)
	}
}
