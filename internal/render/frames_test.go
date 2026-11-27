package render

import (
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/model"
)

func sampleFor(spec FrameSpec) model.Sample {
	return model.Sample{TrackID: spec.Track, Kind: model.KindVideo,
		Seq: uint64(spec.Index), PTS: spec.TimeUS, Duration: 15_000}
}

func TestPlanFramesProduces120ForThirtyMinutes(t *testing.T) {
	// 30 分钟、每 15 秒一帧 = 120 帧。
	specs := PlanFrames(30*60*1_000_000, 15_000_000, 120)
	if len(specs) != 120 {
		t.Fatalf("frames = %d, want 120", len(specs))
	}
	if specs[0].TimeUS != 0 || specs[119].TimeUS != 119*15_000_000 {
		t.Fatalf("frame timing wrong: first=%d last=%d", specs[0].TimeUS, specs[119].TimeUS)
	}
}

func TestDrawIsDeterministic(t *testing.T) {
	spec := FrameSpec{Index: 7, TimeUS: 105_000_000, Seed: 105_000_000, Track: 1}
	img1 := Draw(spec, sampleFor(spec))
	img2 := Draw(spec, sampleFor(spec))
	if PixelHash(img1) != PixelHash(img2) {
		t.Fatal("same spec must produce identical pixel hash")
	}
	if got := len(PixelHash(img1)); got != 64 {
		t.Fatalf("pixel hash length = %d, want 64 hex chars", got)
	}
	// 不同帧索引必须产生不同画面（避免恒等渲染使哈希失去意义）。
	other := FrameSpec{Index: 8, TimeUS: 120_000_000, Seed: 120_000_000, Track: 1}
	if PixelHash(img1) == PixelHash(Draw(other, sampleFor(other))) {
		t.Fatal("different frames must hash differently")
	}
}

func TestExportAndRehashRoundTrip(t *testing.T) {
	dir := t.TempDir()
	specs := PlanFrames(60_000_000, 15_000_000, 120) // 60s -> 4 帧
	refs, err := Export(dir, specs, sampleFor)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 4 {
		t.Fatalf("exported frames = %d, want 4", len(refs))
	}
	for _, ref := range refs {
		if _, err := os.Stat(ref.Path); err != nil {
			t.Fatalf("frame file missing: %v", err)
		}
		bmpPath := ref.Path[:len(ref.Path)-len(".png")] + ".bmp"
		if _, err := os.Stat(bmpPath); err != nil {
			t.Fatalf("bmp sidecar missing: %v", err)
		}
		rehashed, err := RehashPNG(ref.Path)
		if err != nil {
			t.Fatalf("rehash: %v", err)
		}
		if rehashed != ref.Sha256 {
			t.Fatalf("frame %d pixel hash mismatch: %s != %s", ref.Index, rehashed, ref.Sha256)
		}
		if filepath.Base(ref.Path) != frameName(ref.Index) {
			t.Fatalf("frame naming wrong: %s", ref.Path)
		}
	}
	// 确定性：导出到第二个目录，文件名与哈希必须逐帧一致。
	dir2 := t.TempDir()
	refs2, err := Export(dir2, specs, sampleFor)
	if err != nil {
		t.Fatal(err)
	}
	for i := range refs {
		if refs[i].Sha256 != refs2[i].Sha256 {
			t.Fatalf("frame %d not reproducible across directories", i)
		}
	}
}

func TestPlanFramesGuardsBadInput(t *testing.T) {
	if PlanFrames(0, 1000, 120) != nil {
		t.Fatal("zero duration must yield no frames")
	}
	if PlanFrames(1000, 0, 120) != nil {
		t.Fatal("zero step must yield no frames")
	}
}
