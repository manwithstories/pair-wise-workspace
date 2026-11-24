package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clearliu/mediq/internal/core"
)

func TestCheckpointSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "cp.json")
	s := &Store{Path: path}
	in := Checkpoint{InputSHA256: "abc", ParamsSHA256: "def", CommittedOutputSamples: 4800}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.CommittedOutputSamples != 4800 || got.InputSHA256 != "abc" {
		t.Fatalf("bad checkpoint %+v", got)
	}
}

func TestFailedRenameHookPreservesLastCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	s := &Store{Path: path}
	if err := s.Save(Checkpoint{CommittedOutputSamples: 10}); err != nil {
		t.Fatal(err)
	}

	s.BeforeRename = func(Checkpoint) error { return errors.New("injected disk failure") }
	err := s.Save(Checkpoint{CommittedOutputSamples: 20})
	if err == nil {
		t.Fatal("injected save failure was not returned")
	}
	got, lerr := Load(path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got.CommittedOutputSamples != 10 {
		t.Fatalf("checkpoint changed to %d after failed save", got.CommittedOutputSamples)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".checkpoint-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestInterruptAfterAtomicRenamePersistsProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	s := &Store{
		Path: path,
		AfterRename: func(Checkpoint) error {
			return core.New(core.KindInterrupted, "injected", nil)
		},
	}
	err := s.Save(Checkpoint{CommittedOutputSamples: 77})
	if core.ExitCode(err) != 75 {
		t.Fatalf("err=%v exit=%d, want 75", err, core.ExitCode(err))
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommittedOutputSamples != 77 {
		t.Fatalf("persisted=%d, want 77", got.CommittedOutputSamples)
	}
}

func TestValidateResumeBindsInputAndParams(t *testing.T) {
	cp := &Checkpoint{InputSHA256: "in", ParamsSHA256: "p1"}
	if err := cp.ValidateResume("in", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := cp.ValidateResume("other", "p1"); core.KindOf(err) != core.KindCheckpoint {
		t.Fatalf("input mismatch err=%v", err)
	}
	if err := cp.ValidateResume("in", "p2"); core.KindOf(err) != core.KindConfig {
		t.Fatalf("params mismatch err=%v", err)
	}
}

func TestDigestJSONStable(t *testing.T) {
	type P struct {
		Rate int    `json:"rate"`
		Mode string `json:"mode"`
	}
	a, err := DigestJSON(P{Rate: 48000, Mode: "r128"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DigestJSON(P{Rate: 48000, Mode: "r128"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b || len(a) != 64 {
		t.Fatalf("digests unstable/invalid: %q %q", a, b)
	}
}
