package resume

import (
	"os"
	"path/filepath"
	"testing"

	"archive.local/recover/internal/errs"
)

func TestDigestInputsOrderIndependent(t *testing.T) {
	a := map[string][]byte{"b": []byte("two"), "a": []byte("one")}
	b := map[string][]byte{"a": []byte("one"), "b": []byte("two")}
	if DigestInputs(a) != DigestInputs(b) {
		t.Fatal("digest depends on map iteration order")
	}
	c := map[string][]byte{"a": []byte("one"), "b": []byte("THREE")}
	if DigestInputs(a) == DigestInputs(c) {
		t.Fatal("digest ignores content")
	}
}

func TestDigestConfigStable(t *testing.T) {
	type cfg struct {
		A int
		B string
	}
	x, err := DigestConfig(cfg{1, "x"})
	if err != nil {
		t.Fatal(err)
	}
	y, _ := DigestConfig(cfg{1, "x"})
	z, _ := DigestConfig(cfg{2, "x"})
	if x != y || x == z {
		t.Fatalf("config digest unstable: %s %s %s", x, y, z)
	}
}

func TestResumeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := &State{InputDigest: "in", ConfigDigest: "cfg"}
	s.MarkCompleted("scan", "", nil)
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsCompleted("scan") || got.InputDigest != "in" {
		t.Fatalf("state=%+v", got)
	}
	d := Check(got, "in", "cfg")
	if !d.Reuse {
		t.Fatalf("expected reuse: %+v", d)
	}
}

func TestResumeRefusesChangedInput(t *testing.T) {
	prev := &State{InputDigest: "old", ConfigDigest: "cfg"}
	d := Check(prev, "new", "cfg")
	if d.Reuse {
		t.Fatal("reused with changed input")
	}
	if !d.Issues.Has(errs.CodeDigestInput) {
		t.Fatalf("wrong classification: %v", d.Issues)
	}
	if !d.Issues.HasFatal() {
		t.Fatal("digest mismatch must be fatal")
	}
}

func TestResumeRefusesChangedConfig(t *testing.T) {
	prev := &State{InputDigest: "in", ConfigDigest: "old"}
	d := Check(prev, "in", "new")
	if d.Reuse || !d.Issues.Has(errs.CodeDigestConfig) {
		t.Fatalf("decision=%+v", d)
	}
}

func TestResumeRefusesMissingArtifact(t *testing.T) {
	prev := &State{InputDigest: "in", ConfigDigest: "cfg", Artifacts: map[string]Artifact{
		"manifest": {Path: filepath.Join(t.TempDir(), "gone.json"), Digest: "x"},
	}}
	d := Check(prev, "in", "cfg")
	if d.Reuse {
		t.Fatal("reused with missing artifact")
	}
}

func TestNoPreviousRun(t *testing.T) {
	d := Check(nil, "in", "cfg")
	if d.Reuse {
		t.Fatal("reused without state")
	}
	if len(d.Issues) != 0 {
		t.Fatalf("fresh run should not report issues: %v", d.Issues)
	}
}

func TestLoadMissingIsNotError(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || s != nil {
		t.Fatalf("s=%v err=%v", s, err)
	}
}

func TestLoadRejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.json")
	os.WriteFile(p, []byte(`{"version":99}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("unsupported version accepted")
	}
}

func TestReadAllEnforcesLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.bin")
	os.WriteFile(p, make([]byte, 100), 0o644)
	if _, err := ReadAll(p, 10); err == nil {
		t.Fatal("oversized input accepted")
	}
	b, err := ReadAll(p, 100)
	if err != nil || len(b) != 100 {
		t.Fatalf("b=%d err=%v", len(b), err)
	}
}
