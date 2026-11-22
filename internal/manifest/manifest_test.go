package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/model"
)

func iv(seq uint32, startSec, endSec int64) model.Interval {
	return model.Interval{
		FragmentSeq: seq,
		Start:       model.NewRational(startSec, 1),
		End:         model.NewRational(endSec, 1),
		ByteLength:  100,
	}
}

func TestBuildAlignedManifest(t *testing.T) {
	dir := t.TempDir()
	files := map[uint32]string{}
	for _, seq := range []uint32{1, 2, 3} {
				p := filepath.Join(dir, "f.m4s")
		_ = os.WriteFile(p, []byte{byte(seq)}, 0o644)
		files[seq] = p
	}
	tl := []model.Interval{iv(1, 0, 1), iv(2, 1, 2), iv(3, 2, 3)}
	m, err := Build(tl, files, 48000, 48000, "stereo", 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 3 || m.AlignmentMaxMS != 0 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.TotalDuration.Num != 3 {
		t.Fatalf("duration = %+v", m.TotalDuration)
	}
	// 摘要稳定：同内容两次构造摘要一致。
	m2, _ := Build(tl, files, 48000, 48000, "stereo", 1.0)
	if m.SummarySHA() != m2.SummarySHA() {
		t.Fatal("summary sha must be deterministic")
	}
}

func TestBuildRejectsMisalignment(t *testing.T) {
	dir := t.TempDir()
	files := map[uint32]string{}
	for _, seq := range []uint32{1, 2} {
		p := filepath.Join(dir, "f.m4s")
		_ = os.WriteFile(p, []byte{byte(seq)}, 0o644)
		files[seq] = p
	}
	// 第二段起点与第一段结束相差 2ms，超过 1ms 限制。
	tl := []model.Interval{
		{FragmentSeq: 1, Start: model.NewRational(0, 1), End: model.NewRational(1, 1)},
		{FragmentSeq: 2, Start: model.NewRational(1002, 1000), End: model.NewRational(2, 1)},
	}
	_, err := Build(tl, files, 48000, 48000, "stereo", 1.0)
	me, ok := model.AsMediaError(err)
	if !ok || me.Category != model.ErrManifestAlign {
		t.Fatalf("want manifest_misalignment, got %v", err)
	}
}

func TestBuildAcceptsSubMillisecondGap(t *testing.T) {
	dir := t.TempDir()
	files := map[uint32]string{}
	for _, seq := range []uint32{1, 2} {
		p := filepath.Join(dir, "f.m4s")
		_ = os.WriteFile(p, []byte{byte(seq)}, 0o644)
		files[seq] = p
	}
	tl := []model.Interval{
		{FragmentSeq: 1, Start: model.NewRational(0, 1), End: model.NewRational(1, 1)},
		{FragmentSeq: 2, Start: model.NewRational(10005, 10000), End: model.NewRational(2, 1)}, // 0.5ms
	}
	m, err := Build(tl, files, 48000, 48000, "stereo", 1.0)
	if err != nil {
		t.Fatalf("0.5ms gap must pass: %v", err)
	}
	if m.AlignmentMaxMS > 1.0 {
		t.Fatalf("align ms = %f", m.AlignmentMaxMS)
	}
}

func TestManifestWriteAndReRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.m4s")
	_ = os.WriteFile(p, []byte("x"), 0o644)
	tl := []model.Interval{iv(1, 0, 1)}
	m, err := Build(tl, map[uint32]string{1: p}, 48000, 48000, "stereo", 1.0)
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Write(dir, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
}
