package verify

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/render"
	"bcast/internal/resume"
)

func makeRefs(t *testing.T, dir string, n int) []model.FrameRef {
	t.Helper()
	specs := render.PlanFrames(int64(n)*15_000_000, 15_000_000, 120)
	sample := func(s render.FrameSpec) model.Sample {
		return model.Sample{TrackID: 0, Kind: model.KindVideo, Seq: uint64(s.Index), PTS: s.TimeUS}
	}
	refs, err := render.Export(dir, specs, sample)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestAggregatePixelHashStableAnd64Hex(t *testing.T) {
	dir := t.TempDir()
	refs := makeRefs(t, dir, 4)
	h1 := AggregatePixelHash(refs)
	h2 := AggregatePixelHash(append([]model.FrameRef(nil), refs...))
	if h1 != h2 {
		t.Fatal("aggregate hash must be order-stable for sorted refs")
	}
	if len(h1) != 64 {
		t.Fatalf("hash length = %d, want 64", len(h1))
	}
	// 乱序输入也必须得到相同聚合值（函数内部排序）。
	shuffled := []model.FrameRef{refs[3], refs[1], refs[0], refs[2]}
	if AggregatePixelHash(shuffled) != h1 {
		t.Fatal("aggregate hash must be independent of input order")
	}
}

func TestVerifyFramesDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	refs := makeRefs(t, dir, 3)
	bad, err := VerifyFrames(refs)
	if err != nil || bad != 0 {
		t.Fatalf("clean frames bad=%d err=%v", bad, err)
	}
	// 篡改其中一帧 PNG 的像素：用空白图覆盖。
	target := refs[1].Path
	if err := os.WriteFile(target, []byte("not-a-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFrames(refs); err == nil {
		t.Fatal("corrupted PNG must fail decode verification")
	}
}

func TestEvaluateExitSemantics(t *testing.T) {
	ok := Report{PackagesIdentical: true, BlockVerificationOK: true, FrameVerificationOK: true}
	if err := Evaluate(ok); err != nil {
		t.Fatalf("all-good report must pass, got %v", err)
	}
	boundary := ok
	boundary.BoundaryMismatches = 1
	if !errors.Is(Evaluate(boundary), fault.ErrBoundary) {
		t.Fatal("boundary mismatch must map to ErrBoundary")
	}
	cross := ok
	cross.CrossLayerDeviations = 2
	if !errors.Is(Evaluate(cross), fault.ErrBoundary) {
		t.Fatal("cross-layer deviation must map to ErrBoundary")
	}
	checksum := ok
	checksum.PackagesIdentical = false
	if !errors.Is(Evaluate(checksum), fault.ErrChecksum) {
		t.Fatal("package mismatch must map to ErrChecksum")
	}
}

func TestWriteReportDeterministic(t *testing.T) {
	dir := t.TempDir()
	r := Report{Tracks: 5, Renditions: 3, Frames: 120, PixelSHA256: "ab", FullSHA256: "cd",
		ResumedSHA256: "cd", PackagesIdentical: true, BlockVerificationOK: true,
		FrameVerificationOK: true, DriftPPM: []int64{120, -80, 40, 60, -45}}
	p1 := filepath.Join(dir, "a", "report.json")
	p2 := filepath.Join(dir, "b", "report.json")
	if err := WriteReport(p1, r); err != nil {
		t.Fatal(err)
	}
	if err := WriteReport(p2, r); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	if string(b1) != string(b2) {
		t.Fatal("report serialization must be deterministic")
	}
}

func TestComparePackagesAndVerifyPackage(t *testing.T) {
	dir := t.TempDir()
	// 用 resume.Writer 造两个逐字节相同的封装。
	samples := []model.Sample{{TrackID: 0, Seq: 0, PTS: 0, Duration: 1000}}
	gaps := []model.Gap{}
	src := "abcd" + repeatHex("0", 60)
	build := func(name string) (string, *resume.Journal) {
		mpk := filepath.Join(dir, name)
		jp := mpk + ".json"
		w, err := resume.NewFullWriter(mpk, jp, src, 5000, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteAll(samples, gaps, nil,
			resume.Manifest{SegmentUS: 5000, SampleCount: 1, SourceSHA256: src}); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		j, err := resume.Load(jp)
		if err != nil {
			t.Fatal(err)
		}
		return mpk, j
	}
	full, jFull := build("full.mpk")
	resumed, jResumed := build("resumed.mpk")
	hashFull, err := VerifyPackage(full, jFull)
	if err != nil {
		t.Fatalf("VerifyPackage full: %v", err)
	}
	if _, err := VerifyPackage(resumed, jResumed); err != nil {
		t.Fatalf("VerifyPackage resumed: %v", err)
	}
	f, r2, identical, err := ComparePackages(full, resumed)
	if err != nil {
		t.Fatal(err)
	}
	if !identical || f != hashFull || f != r2 || len(f) != 64 {
		t.Fatalf("identical builds must compare equal: %s vs %s", f, r2)
	}
}

func repeatHex(s string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = s[0]
	}
	return string(b)
}
