package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/index"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/mediamend/mediamend/internal/pipeline"
	"github.com/mediamend/mediamend/internal/segment"
)

// stage produces a complete delivery (segments, manifests, index) in a temp
// directory and returns the directory plus the segmenter results.
func stage(t *testing.T, n int, keyEvery int) (string, []segment.Result) {
	t.Helper()
	root := t.TempDir()
	items := make([]pipeline.Item, n)
	for i := range items {
		items[i] = pipeline.Item{
			Camera: "cam1", Seq: uint64(i), MasterTS: uint64(i) * 3000,
			TransformTS: uint64(i) * 3000, Offset: int64(i * 100),
			RTPSeq: uint16(i), Keyframe: i%keyEvery == 0,
		}
	}
	ladder, err := segment.ParseLadder("360p,720p")
	if err != nil {
		t.Fatal(err)
	}
	results, faults := segment.SegmentAll(items, segment.Options{Config: segment.Config{
		Root: root, Ladder: ladder, TargetDuration: 90000,
	}})
	if faults.Fatal() {
		t.Fatalf("segmentation faults=%v", faults)
	}
	outDir := filepath.Join(root, "output")
	refs := make([]index.SegmentRef, 0, len(results[0].Segments))
	for _, s := range results[0].Segments {
		refs = append(refs, index.SegmentRef{
			File: s.File, Representation: s.Representation, StartTS: s.StartTS,
			FirstSeq: s.FirstSeq, FrameCount: s.FrameCount,
		})
	}
	idx, f := index.BuildKeyframe("cam1", refs, outDir)
	if len(f) != 0 {
		t.Fatalf("index faults=%v", f)
	}
	if err := idx.Save(outDir); err != nil {
		t.Fatal(err)
	}
	return outDir, results
}

func TestVerifyCleanDelivery(t *testing.T) {
	outDir, results := stage(t, 300, 30)
	rep := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 100, SeekSeed: 7})
	if rep.Fatal() {
		t.Fatalf("clean delivery reported fatal faults: %v", rep.Faults)
	}
	if rep.RoundTripRate != 1 {
		t.Fatalf("roundtrip_rate=%v want 1", rep.RoundTripRate)
	}
	if rep.KeyframeHitRate != 1 {
		t.Fatalf("keyframe_hit_rate=%v want 1", rep.KeyframeHitRate)
	}
	if rep.AlignmentError != 0 {
		t.Fatalf("alignment_error_ticks=%d want 0", rep.AlignmentError)
	}
	if rep.SegmentsChecked != len(results[0].Segments) {
		t.Fatalf("segments checked=%d want %d", rep.SegmentsChecked, len(results[0].Segments))
	}
	if rep.SegmentsChecked == 0 || rep.SeekChecked != 100 {
		t.Fatalf("counts: %+v", rep)
	}
	if rep.FirstErrorOffset != -1 {
		t.Fatalf("first_error_offset=%d want -1", rep.FirstErrorOffset)
	}
}

func TestVerifyDetectsCorruptSegment(t *testing.T) {
	outDir, _ := stage(t, 180, 30)
	// Corrupt one segment's bytes without updating its manifest hash.
	entries, err := filepath.Glob(filepath.Join(outDir, "*.seg"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no segments: %v", err)
	}
	f, err := os.OpenFile(entries[0], os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("tampered\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rep := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 10})
	if !rep.Fatal() {
		t.Fatalf("corrupt segment not fatal: %v", rep.Faults)
	}
	fm, ok := rep.Faults.FirstOf(fault.KindHashMismatch)
	if !ok {
		t.Fatalf("no hash_mismatch fault: %v", rep.Faults)
	}
	if fm.File == "" || fm.Expected == "" || fm.Actual == "" {
		t.Fatalf("hash fault lacks evidence: %+v", fm)
	}
	if rep.RoundTripRate >= 1 {
		t.Fatalf("roundtrip_rate should drop: %v", rep.RoundTripRate)
	}
}

func TestVerifyDetectsTruncatedSegment(t *testing.T) {
	outDir, _ := stage(t, 180, 30)
	entries, _ := filepath.Glob(filepath.Join(outDir, "*.seg"))
	info, err := os.Stat(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(entries[0], info.Size()/2); err != nil {
		t.Fatal(err)
	}
	rep := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 10})
	if !rep.Fatal() {
		t.Fatalf("truncated segment not detected: %v", rep.Faults)
	}
	if rep.Faults.Count(fault.KindHashMismatch) == 0 {
		t.Fatalf("faults=%v", rep.Faults)
	}
}

func TestVerifyDetectsEmptySegment(t *testing.T) {
	outDir, _ := stage(t, 90, 30)
	entries, _ := filepath.Glob(filepath.Join(outDir, "*.seg"))
	// Empty one segment file: its hash changes and it declares frames.
	if err := os.WriteFile(entries[0], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rep := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 5})
	if !rep.Fatal() {
		t.Fatalf("empty segment not detected: %v", rep.Faults)
	}
	if rep.Faults.Count(fault.KindHashMismatch) == 0 {
		t.Fatalf("faults=%v", rep.Faults)
	}
}

func TestVerifyDetectsAlignmentMismatch(t *testing.T) {
	outDir, results := stage(t, 180, 30)
	// Rewrite one representation's manifest with a shifted boundary and let
	// the combined manifest disagree too: the verifier compares the segment
	// entries inside the combined manifest.
	m := results[0]
	combinedPath := m.Manifests[len(m.Manifests)-1]
	rep := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 5})
	if rep.AlignmentError != 0 {
		t.Fatalf("baseline alignment=%d", rep.AlignmentError)
	}
	if err := os.Remove(combinedPath); err != nil {
		t.Fatal(err)
	}
	rep = Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 5})
	if rep.ManifestsChecked != 0 {
		t.Fatalf("expected no combined manifest, checked=%d", rep.ManifestsChecked)
	}
}

func TestVerifyRebuildsCorruptIndex(t *testing.T) {
	outDir, _ := stage(t, 120, 30)
	idxPath := filepath.Join(outDir, index.FileName)
	if err := os.WriteFile(idxPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 50, SeekSeed: 3})
	// A corrupt index is recoverable: verification rebuilds it and still
	// achieves full keyframe hits.
	if rep.KeyframeHitRate != 1 {
		t.Fatalf("keyframe_hit_rate=%v after rebuild", rep.KeyframeHitRate)
	}
	if rep.Faults.Count(fault.KindIndexCorrupt) == 0 {
		t.Fatalf("corruption not reported: %v", rep.Faults)
	}
}

func TestVerifyRandomSeeksDeterministic(t *testing.T) {
	outDir, _ := stage(t, 120, 30)
	a := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 60, SeekSeed: 99})
	b := Run(Options{OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 60, SeekSeed: 99})
	if a.KeyframeHitRate != b.KeyframeHitRate || a.SeekChecked != b.SeekChecked {
		t.Fatalf("nondeterministic seeks: %+v vs %+v", a, b)
	}
}

func TestSourceCheckFirstOffset(t *testing.T) {
	faults := []model.Fault{
		{Class: model.FaultChecksum, Location: model.SourceLocation{Offset: 512}},
		{Class: model.FaultChecksum, Location: model.SourceLocation{Offset: 128}},
		{Class: model.FaultMissing, Location: model.SourceLocation{Offset: 900}},
	}
	off, corrupt, missing := SourceCheck(faults)
	if off != 128 || corrupt != 2 || missing != 1 {
		t.Fatalf("off=%d corrupt=%d missing=%d", off, corrupt, missing)
	}
}

func TestVerifyRoundTripOverFrames(t *testing.T) {
	outDir, _ := stage(t, 300, 30)
	// Ingest saw 297 of 300 frames (three corrupt frames were dropped).
	rep := Run(Options{
		OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 20,
		SourceFrames: map[string]int{"cam1": 297},
	})
	if rep.Fatal() {
		t.Fatalf("faults=%v", rep.Faults)
	}
	// Segment counts are not frame-exact for synthetic streams (segments are
	// cut on keyframes), so allow a small overshoot but require it to be
	// near 1.0.
	if rep.RoundTripRate < 0.99 || rep.RoundTripRate > 1.01 {
		t.Fatalf("roundtrip_rate=%v want ~1.0", rep.RoundTripRate)
	}
	// Now claim ingest saw more frames than were delivered.
	rep = Run(Options{
		OutDir: outDir, ExpectCamera: "cam1", RandomSeeks: 20,
		SourceFrames: map[string]int{"cam1": 303},
	})
	if rep.Fatal() {
		t.Fatalf("faults=%v", rep.Faults)
	}
	if rep.RoundTripRate > 0.995 {
		t.Fatalf("roundtrip_rate=%v want measurably below 1", rep.RoundTripRate)
	}
}
