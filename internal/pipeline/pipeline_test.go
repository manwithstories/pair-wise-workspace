package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/fault"
	"mediapack/internal/fixture"
	"mediapack/internal/index"
	"mediapack/internal/manifest"
)

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func buildOpts(input, out string) BuildOptions {
	return BuildOptions{
		Input: input, Out: out,
		SegmentMs: 2000, ToleranceMs: 80,
		CheckpointSamples: 128,
		TargetBitrates:    []uint64{450000, 900000},
		Policy:            fault.PolicyStrict,
	}
}

func TestBuildAndVerifyValid(t *testing.T) {
	dir := setup(t)
	input := filepath.Join(dir, "valid.mov")
	out := filepath.Join(dir, "out")
	res, err := Build(buildOpts(input, out))
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocking {
		t.Fatal("valid input must not block")
	}
	m := res.Manifest
	if len(m.Segments) != 5 {
		t.Fatalf("segments: %d", len(m.Segments))
	}
	if len(m.Representations) != 2 {
		t.Fatalf("representations: %d", len(m.Representations))
	}
	if m.MaxAlignmentErrorMs > 80 {
		t.Fatalf("alignment error exceeds tolerance: %f", m.MaxAlignmentErrorMs)
	}
	if m.CheckpointRecovered {
		t.Fatal("fresh build must not be marked recovered")
	}
	// Completion marker written.
	if _, err := os.Stat(filepath.Join(out, manifest.CompleteName)); err != nil {
		t.Fatal("completion marker missing")
	}
	// Verify passes against the same input.
	if _, _, err := Verify(input, filepath.Join(out, manifest.Name)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// A repeated run produces an identical digest.
	out2 := filepath.Join(dir, "out2")
	res2, err := Build(buildOpts(input, out2))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Manifest.Digest != m.Digest {
		t.Fatal("repeated runs must produce identical digests")
	}
}

func TestCheckpointFinalShortBlock(t *testing.T) {
	dir := setup(t)
	input := filepath.Join(dir, "valid.mov")
	out := filepath.Join(dir, "out")
	if _, err := Build(buildOpts(input, out)); err != nil {
		t.Fatal(err)
	}
	cp, err := index.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	// 1069 samples total = 8*128 + 45: the short final block must persist.
	if cp.SamplesProcessed != 1069 {
		t.Fatalf("samples processed: %d", cp.SamplesProcessed)
	}
	if cp.CommittedBoundaries() != 5 {
		t.Fatalf("committed boundaries: %d", cp.CommittedBoundaries())
	}
}

func TestBuildTruncatedSkipAndResume(t *testing.T) {
	dir := setup(t)
	input := filepath.Join(dir, "truncated.mov")
	out := filepath.Join(dir, "out")
	o := buildOpts(input, out)
	o.Policy = fault.PolicySkip
	o.Resume = true
	res, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocking {
		t.Fatal("skip policy must tolerate truncation")
	}
	if len(res.Manifest.MissingIntervals) == 0 {
		t.Fatal("truncated input must report missing intervals")
	}
	if res.Manifest.Quality.Skipped == 0 {
		t.Fatal("skipped samples must be counted")
	}
	// Second run with --resume recovers from the committed checkpoint.
	res2, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Manifest.CheckpointRecovered {
		t.Fatal("resume must be marked as recovered")
	}
	if res2.Manifest.Digest != res.Manifest.Digest {
		t.Fatal("resumed run must reproduce the same digest")
	}
}

func TestBuildCorruptStrictBlocks(t *testing.T) {
	dir := setup(t)
	input := filepath.Join(dir, "corrupt.mov")
	res, err := Build(buildOpts(input, filepath.Join(dir, "out")))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Blocking {
		t.Fatal("strict policy must block on corrupt input")
	}
}

func TestBuildCorruptSkip(t *testing.T) {
	dir := setup(t)
	input := filepath.Join(dir, "corrupt.mov")
	o := buildOpts(input, filepath.Join(dir, "out"))
	o.Policy = fault.PolicySkip
	res, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocking {
		t.Fatal("skip policy must tolerate corrupt input")
	}
	if res.Manifest.Quality.Skipped != 2 {
		t.Fatalf("skipped: %d", res.Manifest.Quality.Skipped)
	}
}

func TestResumeMismatchRejected(t *testing.T) {
	dir := setup(t)
	out := filepath.Join(dir, "out")
	o := buildOpts(filepath.Join(dir, "truncated.mov"), out)
	o.Policy = fault.PolicySkip
	if _, err := Build(o); err != nil {
		t.Fatal(err)
	}
	// Resume with a different input must be refused with ErrMismatch.
	o.Input = filepath.Join(dir, "valid.mov")
	o.Resume = true
	if _, err := Build(o); !errors.Is(err, index.ErrMismatch) {
		t.Fatalf("expected checkpoint mismatch, got %v", err)
	}
	// Resume with a changed configuration must also be refused.
	o.Input = filepath.Join(dir, "truncated.mov")
	o.SegmentMs = 4000
	if _, err := Build(o); !errors.Is(err, index.ErrMismatch) {
		t.Fatalf("expected config mismatch, got %v", err)
	}
}

func TestVerifyMismatch(t *testing.T) {
	dir := setup(t)
	input := filepath.Join(dir, "valid.mov")
	out := filepath.Join(dir, "out")
	if _, err := Build(buildOpts(input, out)); err != nil {
		t.Fatal(err)
	}
	// Verifying a different input against the manifest must fail.
	if _, _, err := Verify(filepath.Join(dir, "noindex.mov"),
		filepath.Join(out, manifest.Name)); !errors.Is(err, ErrManifestMismatch) {
		t.Fatalf("expected manifest mismatch, got %v", err)
	}
}

func TestBuildRejectsOutputInInputDir(t *testing.T) {
	dir := setup(t)
	if _, err := Build(buildOpts(filepath.Join(dir, "valid.mov"), dir)); err == nil {
		t.Fatal("must refuse to write into the input directory")
	}
}
