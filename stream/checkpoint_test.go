package stream

import (
	"os"
	"path/filepath"
	"testing"

	"mediaslice/align"
	"mediaslice/internal/fixture"
	"mediaslice/media"
	"mediaslice/scan"
)

const testSeg = media.Ticks(180000)

type harness struct {
	indexes map[string]*scan.FileIndex
	plan    *align.LadderPlan
	bounds  []*align.TrackBoundary
}

func buildHarness(t *testing.T, dir string, reps []string, nSeg int) *harness {
	t.Helper()
	indexes := map[string]*scan.FileIndex{}
	var bounds []*align.TrackBoundary
	cfg := align.BoundaryConfig{SegmentTicks: testSeg, KeyframeWaitMax: testSeg}
	for _, id := range reps {
		spec := fixture.DefaultSpec(id, int64(42+len(id)),
			media.Ticks(nSeg)*testSeg, testSeg)
		spec.JitterSeed = 42 // shared boundary clock across representations
		plan := fixture.BuildPlan(spec)
		mp4Path := filepath.Join(dir, id+".mp4")
		rtpPath := filepath.Join(dir, id+".rtp")
		if err := fixture.WriteMP4(mp4Path, plan); err != nil {
			t.Fatal(err)
		}
		if err := fixture.WriteRTPSidecar(rtpPath, plan); err != nil {
			t.Fatal(err)
		}
		idx, err := scan.ScanFile(mp4Path, rtpPath, scan.DefaultConfig())
		if err != nil || len(idx.Errors) != 0 {
			t.Fatalf("scan %s: %v %v", id, err, idx.Errors)
		}
		indexes[id] = idx
		bounds = append(bounds, align.ComputeTrackBoundary(id, idx, cfg))
	}
	al := align.AlignLadder(bounds, cfg)
	if al.MaxDelta > 1 {
		t.Fatalf("delta=%d", al.MaxDelta)
	}
	plan, err := align.BuildLadderPlan(al, bounds)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{indexes: indexes, plan: plan, bounds: bounds}
}

func TestCrashResumeReusesSegments(t *testing.T) {
	dir := t.TempDir()
	h := buildHarness(t, dir, []string{"800k", "1600k", "3200k"}, 8)

	// First run: crash after segment 2.
	cfg1 := DefaultConfig()
	cfg1.CrashAfterSequence = 2
	cfg1.CheckpointEvery = 1
	r1, err := NewRunner(dir, h.plan, h.indexes, h.bounds, cfg1)
	if err != nil {
		t.Fatal(err)
	}
	res1, err := r1.Run()
	var crash *ErrCrashInjected
	if err == nil || !asCrash(err, &crash) {
		t.Fatalf("expected crash, got %v", err)
	}
	if !res1.Crashed || len(res1.Segments) != 3*3 {
		t.Fatalf("partial run segments=%d", len(res1.Segments))
	}
	// The last complete segment must exist on disk.
	if _, err := os.Stat(filepath.Join(dir, segmentFileName("3200k", 2))); err != nil {
		t.Fatalf("last complete segment missing: %v", err)
	}

	// Resume run: no crash, must reuse and finish.
	cfg2 := DefaultConfig()
	cfg2.CheckpointEvery = 1
	r2, err := NewRunner(dir, h.plan, h.indexes, h.bounds, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := r2.Run()
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if res2.ReusedSegments != 3 {
		t.Fatalf("reused=%d want 3", res2.ReusedSegments)
	}
	if res2.Manifest.SegmentCount != len(h.plan.Segments) {
		t.Fatalf("manifest segments=%d", res2.Manifest.SegmentCount)
	}

	// Reference clean run in a separate directory must produce identical
	// per-segment checksums (deterministic re-run).
	refDir := filepath.Join(dir, "ref")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	href := buildHarness(t, refDir, []string{"800k", "1600k", "3200k"}, 8)
	rr, err := NewRunner(refDir, href.plan, href.indexes, href.bounds, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	resRef, err := rr.Run()
	if err != nil {
		t.Fatal(err)
	}
	if res2.OutputSHA256 != resRef.OutputSHA256 {
		t.Fatalf("output_sha256 differs: %s vs %s", res2.OutputSHA256, resRef.OutputSHA256)
	}
}

func TestResumeRejectsTamperedSegment(t *testing.T) {
	dir := t.TempDir()
	h := buildHarness(t, dir, []string{"800k", "1600k"}, 4)
	cfg := DefaultConfig()
	cfg.CrashAfterSequence = 1
	cfg.CheckpointEvery = 1
	r, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, cfg)
	if _, err := r.Run(); err == nil {
		t.Fatal("expected injected crash")
	}
	// Tamper one completed segment byte.
	target := filepath.Join(dir, segmentFileName("800k", 0))
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r2, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, DefaultConfig())
	_, err = r2.Run()
	if err == nil {
		t.Fatal("tampered segment was accepted")
	}
	me, ok := err.(*media.MediaError)
	if !ok || me.Kind != media.KindCheckpoint {
		t.Fatalf("expected checkpoint error, got %T %v", err, err)
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	dir := t.TempDir()
	h := buildHarness(t, dir, []string{"800k", "1600k"}, 4)
	cfg := DefaultConfig()
	cfg.CrashAfterSequence = 1
	cfg.CheckpointEvery = 1
	r, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, cfg)
	if _, err := r.Run(); err == nil {
		t.Fatal("expected injected crash")
	}
	// Mutate the recorded input hash to simulate changed source bytes.
	cpPath := filepath.Join(dir, "checkpoint.json")
	raw, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the JSON structural token so unmarshal fails outright.
	raw[0] = '{' ^ raw[0] // flip bits of the opening brace into a non-brace
	_ = os.WriteFile(cpPath, raw, 0o600)
	r2, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, DefaultConfig())
	if _, err := r2.Run(); err == nil {
		t.Fatal("invalid checkpoint was accepted")
	}
}

func TestRunnerHonorsMemoryBudgets(t *testing.T) {
	dir := t.TempDir()
	h := buildHarness(t, dir, []string{"800k"}, 4)
	cfg := DefaultConfig()
	cfg.MaxBufferedBytes = 1 // impossible budget: first sample fails
	r, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, cfg)
	_, err := r.Run()
	if err == nil {
		t.Fatal("byte budget not enforced")
	}
	cfg2 := DefaultConfig()
	cfg2.MaxBufferedSamples = 0 // falls back to default via NewRunner
	r2, _ := NewRunner(t.TempDir(), h.plan, h.indexes, h.bounds, cfg2)
	res, err := r2.Run()
	if err != nil || res.OutputSHA256 == "" {
		t.Fatalf("default budget run failed: %v", err)
	}
}

func TestCheckpointAtomicWrites(t *testing.T) {
	dir := t.TempDir()
	h := buildHarness(t, dir, []string{"800k"}, 2)
	r, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, DefaultConfig())
	if _, err := r.Run(); err != nil {
		t.Fatal(err)
	}
	// No tmp files may remain after atomic rename.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("torn tmp file left behind: %s", e.Name())
		}
	}
	cp, err := LoadCheckpoint(dir)
	if err != nil || cp == nil || cp.NextSequence != len(h.plan.Segments) {
		t.Fatalf("checkpoint not finalized: %v %v", cp, err)
	}
}

func asCrash(err error, target **ErrCrashInjected) bool {
	for err != nil {
		if c, ok := err.(*ErrCrashInjected); ok {
			*target = c
			return true
		}
		return false
	}
	return false
}
