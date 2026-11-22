package pipeline_test

import (
	"path/filepath"
	"testing"

	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
	"mediarun/internal/metadata"
	"mediarun/internal/pipeline"
	"mediarun/testdata"
)

const (
	videoN = 120
	audioN = 120
)

func baseSpec(path, format, injection string) testdata.FixtureSpec {
	return testdata.FixtureSpec{
		Path: path, Format: format,
		VideoSamples: videoN, AudioSamples: audioN,
		VideoScale: 30000, FrameDur: 1001, Width: 320, Height: 180,
		AudioRate: 48000, Injection: injection,
	}
}

func mappingCfg() mapping.Config {
	return mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/0"},
		{Target: mapping.RoleAudio, Source: "audio/1"},
	}}
}

func runOpts(workDir, input string, interrupt int64, resume bool) pipeline.Options {
	return pipeline.Options{
		WorkDir: workDir, InputPath: input,
		OutputName: "out.mrmx", ManifestName: "trace.json",
		QueueDepth: 4, TargetBitrate: 12_000_000,
		InterruptAfter: interrupt, Resume: resume,
		Mapping: mappingCfg(),
		Metadata: map[string]metadata.Input{
			"video/0": {TrackID: "video/0", CodedWidth: 320, CodedHeight: 180},
		},
	}
}

func TestSuccessfulRunStats(t *testing.T) {
	workDir := t.TempDir()
	input := filepath.Join(workDir, "in.mp4")
	if err := testdata.Write(baseSpec(input, "mp4", "")); err != nil {
		t.Fatal(err)
	}
	stats, err := pipeline.Run(runOpts(workDir, input, 0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.SamplesIn != videoN+audioN || stats.SamplesOut != videoN+audioN {
		t.Fatalf("samples in/out = %d/%d, want %d", stats.SamplesIn, stats.SamplesOut, videoN+audioN)
	}
	if stats.QualityChecked != videoN+audioN || stats.QualityErrors != 0 {
		t.Fatalf("qc = %d/%d", stats.QualityChecked, stats.QualityErrors)
	}
	if !stats.OutputCommitted || stats.OutputSHA256 == "" {
		t.Fatal("output must be committed with a hash")
	}
	if stats.ManifestAlignMillis > 1 {
		t.Fatalf("alignment = %d ms, want <= 1", stats.ManifestAlignMillis)
	}
}

func TestDeterministicHashAcrossRuns(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	in1 := filepath.Join(dir1, "in.mp4")
	in2 := filepath.Join(dir2, "in.mp4")
	_ = testdata.Write(baseSpec(in1, "mp4", ""))
	_ = testdata.Write(baseSpec(in2, "mp4", ""))
	s1, err := pipeline.Run(runOpts(dir1, in1, 0, false))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := pipeline.Run(runOpts(dir2, in2, 0, false))
	if err != nil {
		t.Fatal(err)
	}
	if s1.OutputSHA256 != s2.OutputSHA256 {
		t.Fatalf("identical input+config must hash equal: %s vs %s",
			s1.OutputSHA256, s2.OutputSHA256)
	}
}

func TestInterruptAndResumePreservesHash(t *testing.T) {
	workDir := t.TempDir()
	input := filepath.Join(workDir, "in.mp4")
	if err := testdata.Write(baseSpec(input, "mp4", "")); err != nil {
		t.Fatal(err)
	}
	// First leg: stop after 97 samples with a checkpoint.
	first, err := pipeline.Run(runOpts(workDir, input, 97, false))
	if err != nil {
		t.Fatalf("interrupt leg: %v", err)
	}
	if first.SamplesOut != 97 || first.OutputCommitted {
		t.Fatalf("interrupt stats = %+v", first)
	}
	// Reference full run in an isolated directory.
	refDir := t.TempDir()
	refInput := filepath.Join(refDir, "in.mp4")
	_ = testdata.Write(baseSpec(refInput, "mp4", ""))
	ref, err := pipeline.Run(runOpts(refDir, refInput, 0, false))
	if err != nil {
		t.Fatal(err)
	}
	// Resume leg must complete all samples without duplicates.
	resumed, err := pipeline.Run(runOpts(workDir, input, 0, true))
	if err != nil {
		t.Fatalf("resume leg: %v", err)
	}
	if resumed.SamplesOut != videoN+audioN {
		t.Fatalf("resumed out = %d, want %d", resumed.SamplesOut, videoN+audioN)
	}
	if resumed.DuplicateSamples != 0 {
		t.Fatalf("duplicates = %d, want 0", resumed.DuplicateSamples)
	}
	if resumed.OutputSHA256 != ref.OutputSHA256 {
		t.Fatalf("resumed hash %s != reference %s", resumed.OutputSHA256, ref.OutputSHA256)
	}
	if resumed.ResumeFromSample != 97 {
		t.Fatalf("resume_from = %d, want 97", resumed.ResumeFromSample)
	}
}

func TestCheckpointRejectsChangedInput(t *testing.T) {
	workDir := t.TempDir()
	input := filepath.Join(workDir, "in.mp4")
	_ = testdata.Write(baseSpec(input, "mp4", ""))
	if _, err := pipeline.Run(runOpts(workDir, input, 50, false)); err != nil {
		t.Fatal(err)
	}
	// Change the input content while keeping it a recognizable container, so
	// the run reaches checkpoint verification rather than failing at format
	// sniffing: regenerate with a different frame geometry changes bytes/hash.
	alt := baseSpec(input, "mp4", "")
	alt.Width, alt.Height = 640, 360
	_ = testdata.Write(alt)
	_, err := pipeline.Run(runOpts(workDir, input, 0, true))
	if !mediaerr.Is(err, mediaerr.CodeCheckpointMismatch) {
		t.Fatalf("want CHECKPOINT_MISMATCH, got %v", err)
	}
}

func TestCorruptInputFailsWithoutCommit(t *testing.T) {
	workDir := t.TempDir()
	input := filepath.Join(workDir, "in.mp4")
	_ = testdata.Write(baseSpec(input, "mp4", "truncated-box"))
	stats, err := pipeline.Run(runOpts(workDir, input, 0, false))
	if err == nil {
		t.Fatal("corrupt input must fail")
	}
	if !mediaerr.Is(err, mediaerr.CodeTruncatedBox) &&
		!mediaerr.Is(err, mediaerr.CodeUnknownStructure) {
		t.Fatalf("want truncation/structure error, got %v", err)
	}
	if stats != nil && stats.OutputCommitted {
		t.Fatal("no output may be committed for a corrupt input")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	workDir := t.TempDir()
	opts := runOpts(workDir, "../../etc/passwd", 0, false)
	_, err := pipeline.Run(opts)
	if !mediaerr.Is(err, mediaerr.CodePathTraversal) {
		t.Fatalf("want PATH_TRAVERSAL, got %v", err)
	}
}

func TestMXFEndToEnd(t *testing.T) {
	workDir := t.TempDir()
	input := filepath.Join(workDir, "in.mxf")
	spec := testdata.FixtureSpec{
		Path: input, Format: "mxf", VideoSamples: 60, AudioSamples: 60,
		VideoScale: 25, FrameDur: 1, Width: 640, Height: 360, AudioRate: 48000,
	}
	if err := testdata.Write(spec); err != nil {
		t.Fatal(err)
	}
	stats, err := pipeline.Run(runOpts(workDir, input, 0, false))
	if err != nil {
		t.Fatalf("MXF run: %v", err)
	}
	if stats.Format != "mxf" || stats.SamplesOut != 120 {
		t.Fatalf("mxf stats = %+v", stats)
	}
}
