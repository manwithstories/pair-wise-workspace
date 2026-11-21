package mux

import (
	"context"
	"path/filepath"
	"testing"

	"mediacore/internal/adapter"
	"mediacore/internal/media"
	"mediacore/internal/pipeline"
)

func buildInput(t *testing.T) (media.Source, []*media.Track, map[string]uint32, string) {
	t.Helper()
	layout, err := adapter.BuildMP4(adapter.MP4Spec{Tracks: 3, Samples: 60, Seed: 11,
		Color: &adapter.MediaColor{Primaries: 1, Transfer: 1, Matrix: 1}})
	if err != nil {
		t.Fatal(err)
	}
	src := adapter.NewLogicalSource(layout, "input.mp4")
	c, err := (&adapter.MP4Adapter{}).Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	plan := map[string]uint32{}
	for i, tr := range c.Tracks {
		plan[tr.ID.String()] = uint32(i + 1)
	}
	return src, c.Tracks, plan, c.Fingerprint
}

func TestRemuxPreservesSampleStreams(t *testing.T) {
	src, tracks, plan, fp := buildInput(t)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.mp4")
	r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}
	res, err := r.Run(context.Background(), Options{OutputPath: outPath, Fingerprint: fp, MappingSign: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tracks {
		if !tr.Match() {
			t.Fatalf("track %s input=%s output=%s", tr.Key, tr.InputSHA256, tr.OutputSHA256)
		}
	}

	// Re-parse the output and confirm the sample tables are consistent.
	out, err := media.OpenFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	c, err := (&adapter.MP4Adapter{}).Parse(context.Background(), out)
	if err != nil {
		t.Fatalf("output does not reparse: %v", err)
	}
	if len(c.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(c.Tracks))
	}
	for _, tr := range c.Tracks {
		if tr.SampleCount() != 60 {
			t.Fatalf("track %s samples=%d", tr.ID, tr.SampleCount())
		}
	}
}

func TestRemuxInterruptAndResumeWritesEachSampleOnce(t *testing.T) {
	src, tracks, plan, fp := buildInput(t)
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.mp4")
	cpPath := filepath.Join(dir, "cp.json")
	r := &Remuxer{Tracks: tracks, Plan: plan, Source: src}

	// First run stops partway.
	_, err := r.Run(context.Background(), Options{
		OutputPath: outPath, CheckpointPath: cpPath, Fingerprint: fp,
		MappingSign: "sig", InterruptAfter: 70, WaveSize: 10,
	})
	if err == nil {
		t.Fatal("expected interrupt")
	}
	if _, ok := err.(*InterruptError); !ok {
		t.Fatalf("err=%v", err)
	}

	// Second run resumes without replaying samples.
	planResume, err := pipeline.Load(cpPath, fp, "sig", outPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), Options{
		OutputPath: outPath, CheckpointPath: cpPath, Fingerprint: fp,
		MappingSign: "sig", Resume: planResume, WaveSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 110 {
		t.Fatalf("resumed samples=%d want 110", res.Samples)
	}
	total := 0
	for _, tr := range res.Tracks {
		total += tr.Samples
	}
	if res.Samples+70 != total {
		t.Fatalf("replayed or lost samples: produced=%d total=%d", res.Samples, total)
	}
	for _, tr := range res.Tracks {
		if !tr.Match() {
			t.Fatalf("track %s input=%s output=%s", tr.Key, tr.InputSHA256, tr.OutputSHA256)
		}
	}
}
