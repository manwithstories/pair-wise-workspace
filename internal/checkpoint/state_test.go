package checkpoint_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/checkpoint"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSaveLoadVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	frames := writeFile(t, dir, "frames.log", "frame-data")
	idx := writeFile(t, dir, "track.idx", "index-data")
	fa, err := checkpoint.ArtifactOf(frames)
	if err != nil {
		t.Fatal(err)
	}
	ia, err := checkpoint.ArtifactOf(idx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.ckpt")
	st := checkpoint.State{
		TrackID: 1, NextFrame: 120, Indexed: 120, Stage: checkpoint.StageManifest,
		FramesFile: fa, IndexFile: ia, ManifestSum: "abcd",
	}
	if err := checkpoint.Save(path, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := checkpoint.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.NextFrame != 120 || got.Stage != checkpoint.StageManifest {
		t.Fatalf("loaded %+v", got)
	}
	if err := checkpoint.Verify(got); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestOrderingRejectsPrematureStages(t *testing.T) {
	cases := []checkpoint.State{
		{Stage: checkpoint.StageIndex},
		{Stage: checkpoint.StageManifest, FramesFile: checkpoint.Artifact{Digest: "x"}},
		{Stage: checkpoint.StageManifest, FramesFile: checkpoint.Artifact{Digest: "x"}, IndexFile: checkpoint.Artifact{Digest: "y"}},
		{Stage: "bogus"},
	}
	for i, st := range cases {
		if err := checkpoint.Ordering(st); err == nil {
			t.Fatalf("case %d: ordering accepted premature stage", i)
		}
	}
}

func TestVerifyDetectsChangedArtifact(t *testing.T) {
	dir := t.TempDir()
	frames := writeFile(t, dir, "frames.log", "frame-data")
	fa, _ := checkpoint.ArtifactOf(frames)
	path := filepath.Join(dir, "state.ckpt")
	if err := checkpoint.Save(path, checkpoint.State{
		TrackID: 1, NextFrame: 10, Stage: checkpoint.StageIngest, FramesFile: fa,
	}); err != nil {
		t.Fatal(err)
	}
	st, err := checkpoint.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the frames file after the checkpoint committed.
	if err := os.WriteFile(frames, []byte("frame-data-MUTATED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Verify(st); !errors.Is(err, checkpoint.ErrUnfinished) {
		t.Fatalf("verify err = %v, want ErrUnfinished", err)
	}
}

func TestVerifyDetectsMissingArtifact(t *testing.T) {
	dir := t.TempDir()
	frames := writeFile(t, dir, "frames.log", "x")
	fa, _ := checkpoint.ArtifactOf(frames)
	path := filepath.Join(dir, "state.ckpt")
	if err := checkpoint.Save(path, checkpoint.State{TrackID: 1, NextFrame: 1, Stage: checkpoint.StageIngest, FramesFile: fa}); err != nil {
		t.Fatal(err)
	}
	st, _ := checkpoint.Load(path)
	if err := os.Remove(frames); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Verify(st); !errors.Is(err, checkpoint.ErrUnfinished) {
		t.Fatalf("verify err = %v, want ErrUnfinished", err)
	}
}

func TestSaveRefusesRegression(t *testing.T) {
	dir := t.TempDir()
	frames := writeFile(t, dir, "frames.log", "x")
	fa, _ := checkpoint.ArtifactOf(frames)
	path := filepath.Join(dir, "state.ckpt")
	if err := checkpoint.Save(path, checkpoint.State{TrackID: 1, NextFrame: 100, Stage: checkpoint.StageIngest, FramesFile: fa}); err != nil {
		t.Fatal(err)
	}
	err := checkpoint.Save(path, checkpoint.State{TrackID: 1, NextFrame: 50, Stage: checkpoint.StageIngest, FramesFile: fa})
	if err == nil {
		t.Fatal("save regressed next_frame")
	}
}

func TestLoadMissingIsSentinel(t *testing.T) {
	_, err := checkpoint.Load(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, checkpoint.ErrNoCheckpoint) {
		t.Fatalf("err = %v, want ErrNoCheckpoint", err)
	}
}

func TestLoadRejectsCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "bad.ckpt", "{not json")
	if _, err := checkpoint.Load(p); err == nil {
		t.Fatal("corrupt checkpoint loaded")
	}
	p2 := writeFile(t, dir, "wrong.ckpt", `{"magic":"X","version":9}`)
	if _, err := checkpoint.Load(p2); err == nil {
		t.Fatal("wrong magic/version loaded")
	}
}
