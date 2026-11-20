package pipeline

import (
	"os"
	"testing"

	"github.com/mediamend/mediamend/internal/checkpoint"
	"github.com/mediamend/mediamend/internal/fixture"
	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
)

func buildConfig(t *testing.T, root string, perTrack uint64, every uint64) Config {
	t.Helper()
	proj, corrupt, err := fixture.Generate(fixture.Options{
		Root: root, FramesPerTrack: perTrack, CorruptEvery: every,
		InjectRollover: true, InjectDuplicate: true, InjectBackwards: true,
		InjectMissing: true, InjectColorFault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var inputs []ingest.TrackInput
	for _, s := range proj.Tracks {
		inputs = append(inputs, ingest.TrackInput{TrackID: s.ID, File: s.File})
	}
	return Config{
		Root: root, Project: proj, Inputs: inputs,
		KeyEvery: proj.KeyEvery, MaxWorkers: 4,
		RandomSeeks: 50, SeekSeed: 42, ExpectedCorrupt: corrupt,
	}
}

func TestRunFullPipeline(t *testing.T) {
	cfg := buildConfig(t, t.TempDir(), 300, 97)
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.FramesTotal <= 0 {
		t.Fatal("no frames processed")
	}
	// Every injected checksum fault across all four tracks must be located.
	if res.LocationPrecision != 1.0 {
		t.Fatalf("location precision=%f want 1.0 (located=%d)", res.LocationPrecision, res.CorruptLocated)
	}
	if res.SeekChecked != 50*len(cfg.Project.Tracks) {
		t.Fatalf("seek_checked=%d", res.SeekChecked)
	}
	if res.OutputSHA256 == "" {
		t.Fatal("output checksum missing")
	}
	if res.CompleteTracks != len(cfg.Project.Tracks) {
		t.Fatalf("complete=%d want %d; faults=%d", res.CompleteTracks, len(cfg.Project.Tracks), res.FatalCount)
	}
}

func TestRunDeterministicOutputHash(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	r1, err := Run(buildConfig(t, dir1, 200, 50))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(buildConfig(t, dir2, 200, 50))
	if err != nil {
		t.Fatal(err)
	}
	if r1.OutputSHA256 != r2.OutputSHA256 {
		t.Fatalf("output hash not stable: %s vs %s", r1.OutputSHA256, r2.OutputSHA256)
	}
}

func TestInterruptAndResume(t *testing.T) {
	root := t.TempDir()
	cfg := buildConfig(t, root, 250, 97)
	cfg.InterruptAfter = checkpoint.StageIndex

	if _, err := Run(cfg); err != ErrInterrupted {
		t.Fatalf("want ErrInterrupted, got %v", err)
	}
	store := checkpoint.NewStore(root)
	cp, err := store.Load()
	if err != nil || cp == nil {
		t.Fatalf("checkpoint not persisted: %v", err)
	}
	if !cp.Skip(checkpoint.StageIndex) {
		t.Fatal("index stage not marked complete before interrupt")
	}

	// Resume runs only the remaining manifest stage and must be fast.
	cfg.InterruptAfter = ""
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if !res.Resumed {
		t.Fatal("resume not detected")
	}
	if len(res.SkippedStages) == 0 {
		t.Fatal("no stages skipped on resume")
	}
	if res.OutputSHA256 == "" {
		t.Fatal("manifest not completed on resume")
	}
	// Index stage must have been skipped, proving the expensive stage was reused.
	found := false
	for _, s := range res.SkippedStages {
		if s == checkpoint.StageIndex {
			found = true
		}
	}
	if !found {
		t.Fatalf("index stage not skipped: %v", res.SkippedStages)
	}
}

func TestCheckpointDigestMismatchExit4(t *testing.T) {
	root := t.TempDir()
	cfg := buildConfig(t, root, 120, 50)
	if _, err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	// Mutate one input file so the digest changes; reuse must be refused.
	p := cfg.Inputs[0].File
	if err := appendJunk(p); err != nil {
		t.Fatal(err)
	}
	res, err := Run(cfg)
	if err == nil {
		t.Fatal("digest mismatch should reject resume")
	}
	var cpFault bool
	for _, f := range res.Faults {
		if f.Class == model.FaultCheckpoint {
			cpFault = true
		}
	}
	if !cpFault || model.ExitCode(res.Faults) != 4 {
		t.Fatalf("want checkpoint_mismatch exit 4, got %v", res.Faults)
	}
}

func TestSingleTrackFailureKeepsOthers(t *testing.T) {
	root := t.TempDir()
	// Clean dataset: no checksum corruption and no injected timeline/color
	// defects, so only the deliberately broken a2 layout fails.
	proj, _, err := fixture.Generate(fixture.Options{Root: root, FramesPerTrack: 150})
	if err != nil {
		t.Fatal(err)
	}
	var inputs []ingest.TrackInput
	for _, s := range proj.Tracks {
		inputs = append(inputs, ingest.TrackInput{TrackID: s.ID, File: s.File})
	}
	cfg := Config{
		Root: root, Project: proj, Inputs: inputs,
		KeyEvery: proj.KeyEvery, MaxWorkers: 4, RandomSeeks: 20, SeekSeed: 1,
	}
	// Force one audio track to an invalid layout by editing the project spec.
	for i := range cfg.Project.Tracks {
		if cfg.Project.Tracks[i].ID == "a2" {
			cfg.Project.Tracks[i].Layout = "unknown-99"
		}
	}
	res, err := Run(cfg)
	// A fatal track fault is surfaced but other tracks still produce output.
	if err != nil {
		t.Fatalf("pipeline should isolate track failure, got %v", err)
	}
	if res.Tracks["a2"].Complete {
		t.Fatal("invalid-layout track should be incomplete")
	}
	// a1 is healthy and independent; v1 has no dependency on the failed audio
	// track and must still complete with its own index.
	if !res.Tracks["a1"].Complete || !res.Tracks["v1"].Complete {
		t.Fatalf("independent healthy tracks must still complete: a1=%v v1=%v",
			res.Tracks["a1"].Complete, res.Tracks["v1"].Complete)
	}
	// v2 aligns to the failed a2 and must be blocked (see graph pairing).
	if res.Tracks["v2"].Complete || len(res.Tracks["v2"].BlockedBy) == 0 {
		t.Fatalf("v2 must be marked blocked by failed a2: %+v", res.Tracks["v2"])
	}
	if res.IncompleteTracks < 1 {
		t.Fatal("incomplete track not counted")
	}
}

func appendJunk(p string) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n{\"junk\":true}\n")
	return err
}
