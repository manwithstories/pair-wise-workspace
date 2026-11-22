package pipeline_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/ebml"
	"mediarun/internal/fixture"
	"mediarun/internal/pipeline"
	"mediarun/internal/report"
)

const (
	testSeed   = int64(41)
	testDurNs  = int64(4_000_000_000) // 4 s
	testSegNs  = int64(1_000_000_000) // 1 s segments -> 4 segments
	testBps    = int64(1_800_000)
	testFps    = 2
	testFrames = 8
)

func testOptions(root string) pipeline.Options {
	return pipeline.Options{
		Seed:           testSeed,
		DurationNs:     testDurNs,
		TargetBps:      testBps,
		Fps:            testFps,
		SegmentNs:      testSegNs,
		OutDir:         filepath.Join(root, "out"),
		CacheDir:       filepath.Join(root, "cache"),
		CheckpointPath: filepath.Join(root, "checkpoint.json"),
	}
}

func testInput() pipeline.Input {
	src, tracks := fixture.BuildEBML(testSeed, testDurNs, testBps)
	var tm []report.TrackMap
	for _, t := range tracks {
		tm = append(tm, report.TrackMap{ID: t.ID, Kind: t.Kind})
	}
	return pipeline.Input{
		Source: src,
		Tracks: tm,
		Packets: fixture.Packetize(src, fixture.RTPOptions{
			Duplicates: true, Reorder: true, WrapStart: true,
		}),
	}
}

func runOK(t *testing.T, opts pipeline.Options) *pipeline.Result {
	t.Helper()
	res, err := pipeline.Run(opts, testInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Interrupted {
		t.Fatal("unexpected interrupt")
	}
	return res
}

func TestFullRunManifestFields(t *testing.T) {
	res := runOK(t, testOptions(t.TempDir()))
	m := res.Manifest
	if len(m.Frames) != testFrames {
		t.Fatalf("frames=%d", len(m.Frames))
	}
	if len(m.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(m.Tracks))
	}
	if m.TargetBps != testBps || m.ActualBps != testBps {
		t.Fatalf("bps target=%d actual=%d", m.TargetBps, m.ActualBps)
	}
	if m.BitrateDeltaPct > 1.0 {
		t.Fatalf("delta=%.3f", m.BitrateDeltaPct)
	}
	if m.QualityPSNRdB < 35 {
		t.Fatalf("psnr=%.2f", m.QualityPSNRdB)
	}
	if m.AlignmentMs > 2.0 {
		t.Fatalf("alignment=%.3f", m.AlignmentMs)
	}
	if len(m.Checkpoints) != 4 {
		t.Fatalf("checkpoints=%v", m.Checkpoints)
	}
}

func TestInterruptAndResume(t *testing.T) {
	root := t.TempDir()
	opts := testOptions(root)
	opts.InterruptAfter = 2

	res, err := pipeline.Run(opts, testInput())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Interrupted || res.CheckpointEndMs != 2000 {
		t.Fatalf("interrupted=%v end=%d", res.Interrupted, res.CheckpointEndMs)
	}

	opts.InterruptAfter = 0
	res2 := runOK(t, opts)
	if !res2.Resumed || res2.ReusedSegments < 1 {
		t.Fatalf("resumed=%v reused=%d", res2.Resumed, res2.ReusedSegments)
	}
	if res2.DuplicateCommits != 0 {
		t.Fatalf("duplicate commits=%d", res2.DuplicateCommits)
	}
	if len(res2.Manifest.Frames) != testFrames {
		t.Fatalf("frames=%d", len(res2.Manifest.Frames))
	}
}

func TestRerunDeterministicAndIdempotent(t *testing.T) {
	root := t.TempDir()
	opts := testOptions(root)
	first := runOK(t, opts)
	d1, err := first.Manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}

	second := runOK(t, opts) // same dirs: everything reused
	if second.ReusedSegments != 4 || second.DuplicateCommits != 0 {
		t.Fatalf("reused=%d dup=%d", second.ReusedSegments, second.DuplicateCommits)
	}
	d2, err := second.Manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("manifest digest changed across reruns")
	}

	// Fresh dirs, same seed: frame digests must be identical.
	fresh := runOK(t, testOptions(t.TempDir()))
	for i := range first.Manifest.Frames {
		if first.Manifest.Frames[i].SHA256 != fresh.Manifest.Frames[i].SHA256 {
			t.Fatalf("frame %d digest differs across runs", i)
		}
	}
}

func TestWrongSourceCacheDiscarded(t *testing.T) {
	root := t.TempDir()
	opts := testOptions(root)
	in := testInput()

	// Plant a cache entry for segment 0 with a foreign source digest.
	srcSHA := pipeline.SHA256Of(in.Source)
	key := pipeline.SegmentCacheKey(srcSHA, pipeline.TracksString(in.Tracks),
		"1000000/1", testSeed, testDurNs, testBps, testFps, 0)
	cache := &pipeline.Cache{Dir: opts.CacheDir}
	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := pipeline.Entry{
		SourceSHA: "0000000000000000000000000000000000000000000000000000000000000000",
		ParamsSHA: key.ParamsSHA,
		FrameSHAs: []string{"bad"},
	}
	meta, _ := json.Marshal(&foreign)
	if err := os.WriteFile(cache.FileName(key), meta, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := pipeline.Run(opts, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Manifest.Frames) != testFrames {
		t.Fatalf("frames=%d", len(res.Manifest.Frames))
	}
	// The foreign entry must have been replaced by one bound to our source.
	data, err := os.ReadFile(cache.FileName(key))
	if err != nil {
		t.Fatal(err)
	}
	var got pipeline.Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.SourceSHA != srcSHA {
		t.Fatalf("cache entry still foreign: %s", got.SourceSHA)
	}
}

func TestCorruptedCheckpointQuarantined(t *testing.T) {
	root := t.TempDir()
	opts := testOptions(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.CheckpointPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runOK(t, opts)
	if len(res.Manifest.Frames) != testFrames {
		t.Fatalf("frames=%d", len(res.Manifest.Frames))
	}
	if _, err := os.Stat(opts.CheckpointPath + ".corrupt"); err != nil {
		t.Fatalf("corrupt checkpoint not quarantined: %v", err)
	}
}

func TestTransportLossRejected(t *testing.T) {
	src, tracks := fixture.BuildEBML(testSeed, testDurNs, testBps)
	var tm []report.TrackMap
	for _, tr := range tracks {
		tm = append(tm, report.TrackMap{ID: tr.ID, Kind: tr.Kind})
	}
	in := pipeline.Input{
		Source: src,
		Tracks: tm,
		Packets: fixture.Packetize(src, fixture.RTPOptions{
			WrapStart: true, Loss: []int{10, 11},
		}),
	}
	_, err := pipeline.Run(testOptions(t.TempDir()), in)
	var perr *pipeline.Error
	if e, ok := err.(*pipeline.Error); ok {
		perr = e
	}
	if perr == nil || perr.Code != "TRANSPORT_LOSS" {
		t.Fatalf("err=%v", err)
	}
}

func TestCorruptedEBMLRejected(t *testing.T) {
	bad := fixture.Corrupted()
	in := pipeline.Input{
		Source:  bad,
		Packets: fixture.Packetize(bad, fixture.RTPOptions{}),
	}
	_, err := pipeline.Run(testOptions(t.TempDir()), in)
	perr, ok := err.(*pipeline.Error)
	if !ok {
		t.Fatalf("err=%v", err)
	}
	if perr.Code != string(ebml.ErrTruncatedVINT) || perr.Offset <= 0 {
		t.Fatalf("code=%s offset=%d", perr.Code, perr.Offset)
	}
}
