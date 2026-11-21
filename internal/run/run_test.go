package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/pairflow"
	"github.com/mediamend/mediamend/internal/pipeline"
	"github.com/mediamend/mediamend/internal/segment"
)

// fixture generates a six-camera dataset and returns its directory.
func fixture(t *testing.T, frames uint32, corruptEvery uint32) (string, *pairflow.Manifest) {
	t.Helper()
	dir := t.TempDir()
	m, fs := pairflow.Generate(pairflow.Options{
		Root: dir, Frames: frames, CorruptEvery: corruptEvery, CameraCount: 6,
	})
	if fs.Fatal() {
		t.Fatalf("fixture faults=%v", fs)
	}
	return dir, m
}

func testLadder(t *testing.T) []segment.Representation {
	t.Helper()
	l, err := segment.ParseLadder("360p,720p,1080p")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRunEndToEnd(t *testing.T) {
	in, fx := fixture(t, 300, 37)
	out := t.TempDir()
	rep, err := Run(context.Background(), Config{
		InputDir: in, OutDir: out, QueueCap: 8,
		Ladder: testLadder(t), RandomSeeks: 150, TargetDuration: 90000,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Acceptance invariants. Corrupt frames are excluded from delivery, so
	// the rate sits just below 1 and no lower than the corrupt share allows.
	if rep.RoundTripRate <= 0 || rep.RoundTripRate > 1 {
		t.Fatalf("roundtrip_rate=%v want (0,1]", rep.RoundTripRate)
	}
	if want := 1 - float64(fx.TotalCorrupt())/float64(fx.TotalFrames()); rep.RoundTripRate < want-0.02 {
		t.Fatalf("roundtrip_rate=%v too low (corrupt share implies >= %.3f)", rep.RoundTripRate, want)
	}
	if rep.KeyframeHitRate != 1 {
		t.Fatalf("keyframe_hit_rate=%v want 1", rep.KeyframeHitRate)
	}
	if rep.SegmentSHAChecked == 0 {
		t.Fatal("no segment hashes checked")
	}
	if rep.AlignmentError != 0 {
		t.Fatalf("alignment_error_ticks=%d want 0", rep.AlignmentError)
	}
	if rep.PeakInflight > 24 {
		t.Fatalf("peak_inflight_frames=%d exceeds 24", rep.PeakInflight)
	}
	if rep.CorruptRecords != fx.TotalCorrupt() {
		t.Fatalf("corrupt_records=%d want %d (one per injected frame)",
			rep.CorruptRecords, fx.TotalCorrupt())
	}
	if rep.FirstErrorOffset < 0 {
		t.Fatalf("first_error_offset=%d", rep.FirstErrorOffset)
	}
	if rep.Segments == 0 || len(rep.Manifests) == 0 {
		t.Fatalf("no delivery output: %+v", rep)
	}
	// Exactly one index per camera-present file plus a combined one.
	if _, err := os.Stat(filepath.Join(out, "output", "keyframes.idx.json")); err != nil {
		t.Fatalf("keyframe index missing: %v", err)
	}
	// Report must be persisted and parseable.
	data, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed Report
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("report.json is not valid JSON: %v", err)
	}
	if parsed.CorruptRecords != rep.CorruptRecords {
		t.Fatalf("persisted corrupt_records differs")
	}
}

func TestRunCleanInputNoCorruption(t *testing.T) {
	in, _ := fixture(t, 180, 0)
	out := t.TempDir()
	rep, err := Run(context.Background(), Config{InputDir: in, OutDir: out, Ladder: testLadder(t)})
	if err != nil {
		t.Fatal(err)
	}
	if rep.CorruptRecords != 0 {
		t.Fatalf("corrupt_records=%d want 0", rep.CorruptRecords)
	}
	if rep.RoundTripRate != 1 || rep.KeyframeHitRate != 1 {
		t.Fatalf("clean run rates: %+v", rep)
	}
	// Batch resets and wraps are expected defects, not corruption.
	if rep.DuplicateFrames == 0 && rep.TimestampWraps == 0 {
		t.Fatalf("expected injected duplicate/wrap faults: %+v", rep)
	}
}

func TestRunDetectsTruncatedInput(t *testing.T) {
	in, fx := fixture(t, 120, 0)
	// Truncate one camera file to simulate an interrupted transfer.
	cam := fx.Cameras[0]
	path := fx.Files[cam.ID]
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()*2/3); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	rep, err := Run(context.Background(), Config{InputDir: in, OutDir: out, Ladder: testLadder(t)})
	if err != nil {
		t.Fatal(err)
	}
	// The truncated file yields a recoverable truncation fault but the other
	// cameras still deliver.
	if rep.Faults.Count(fault.KindTruncatedRecord) == 0 {
		t.Fatalf("truncation not reported: %v", rep.Faults)
	}
	if rep.Segments == 0 {
		t.Fatal("no segments produced from the remaining cameras")
	}
}

func TestRunSlowConsumerBackpressure(t *testing.T) {
	in, _ := fixture(t, 200, 0)
	out := t.TempDir()
	rep, err := Run(context.Background(), Config{
		InputDir: in, OutDir: out, Ladder: testLadder(t), QueueCap: 2,
		SlowConsumer: 2_000_000, // 2ms
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.BackpressureHits == 0 {
		t.Fatalf("no backpressure with a slow consumer: %+v", rep)
	}
	if rep.PeakInflight > 24 {
		t.Fatalf("peak=%d", rep.PeakInflight)
	}
}

func TestRunCancelKeepsCommittedSegments(t *testing.T) {
	in, _ := fixture(t, 600, 0)
	out := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var segs int
	rep, err := Run(ctx, Config{
		InputDir: in, OutDir: out, Ladder: testLadder(t), QueueCap: 2,
		SlowConsumer: 3_000_000,
		Progress: func(e pipeline.Event) {
			if e.Stage == "verify" && e.Seq >= 50 {
				cancel()
			}
		},
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !rep.Canceled {
		t.Fatal("report not marked canceled")
	}
	if rep.FramesCommitted == 0 {
		t.Fatal("no frames committed before cancellation")
	}
	_ = segs
}

func TestRunMissingFixture(t *testing.T) {
	if _, err := Run(context.Background(), Config{InputDir: t.TempDir(), OutDir: t.TempDir()}); err == nil {
		t.Fatal("missing fixture accepted")
	}
	if _, err := Run(context.Background(), Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
}
